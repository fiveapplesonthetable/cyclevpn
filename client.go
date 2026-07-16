package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Per-request timeouts. Control ops fail fast so a dropped connection (the
// throttle kills a fraction of new connections) is retried within seconds; the
// download long-poll gets longer since the relay legitimately holds it.
var (
	ctrlTimeout  = 8 * time.Second // session open/upload/close
	fetchTimeout = 2 * time.Second // prefetch of an already-produced chunk (instant on the relay)
	pollTimeout  = 6 * time.Second // long-poll for a not-yet-produced chunk
)

type xport struct {
	host    string // host:port to dial
	hostHdr string // Host header
	tlsCfg  *tls.Config
	pool    chan *tls.Conn // pre-warmed, handshake-done, unused connections
	sem     chan struct{}  // global cap on concurrent in-flight requests
}

// newXport pre-warms a bounded POOL of TLS connections to the relay. Each
// connection carries exactly one request (to stay under the per-connection byte
// quota) and is then discarded. A background warmer keeps the pool full and
// silently absorbs the fraction of connections the throttle drops, so the
// request path only ever grabs a healthy, handshake-already-done connection.
func newXport(base string, insecure bool, serverName string, maxConns, poolSize int) *xport {
	base = strings.TrimRight(base, "/")
	hostHdr := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://"), "/", 2)[0]
	host := hostHdr
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	x := &xport{
		host:    host,
		hostHdr: hostHdr,
		tlsCfg: &tls.Config{
			InsecureSkipVerify: insecure,
			ServerName:         serverName,
			ClientSessionCache: tls.NewLRUClientSessionCache(4096),
			MinVersion:         tls.VersionTLS12,
		},
		pool: make(chan *tls.Conn, poolSize),
		sem:  make(chan struct{}, maxConns),
	}
	for i := 0; i < poolSize/8+1; i++ {
		go x.warm()
	}
	return x
}

// dial opens one fresh TCP+TLS connection (handshake done, no request yet).
func (x *xport) dial(timeout time.Duration) (*tls.Conn, error) {
	raw, err := net.DialTimeout("tcp", x.host, timeout)
	if err != nil {
		return nil, err
	}
	raw.(*net.TCPConn).SetNoDelay(true)
	c := tls.Client(raw, x.tlsCfg)
	c.SetDeadline(time.Now().Add(timeout))
	if err := c.HandshakeContext(context.Background()); err != nil {
		raw.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

// warm keeps the pool topped up with healthy pre-established connections.
func (x *xport) warm() {
	for {
		if len(x.pool) >= cap(x.pool) {
			time.Sleep(40 * time.Millisecond)
			continue
		}
		c, err := x.dial(4 * time.Second)
		if err != nil {
			time.Sleep(50 * time.Millisecond) // dropped by the throttle — back off so we don't hammer the path
			continue
		}
		select {
		case x.pool <- c:
		default:
			c.Close()
		}
	}
}

func (x *xport) getConn(timeout time.Duration) (*tls.Conn, error) {
	select {
	case c := <-x.pool:
		return c, nil
	default:
		return x.dial(timeout)
	}
}

// do sends one request on a fresh/pooled connection and returns the response.
func (x *xport) do(method, path string, body []byte, timeout time.Duration) (int, http.Header, []byte, error) {
	x.sem <- struct{}{}
	defer func() { <-x.sem }()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var c *tls.Conn
		var err error
		if attempt == 0 {
			c, err = x.getConn(timeout) // a pre-warmed connection first (cheap)
		} else {
			// The pooled connection was stale/dead — dial FRESH. Grabbing another pooled
			// connection (which may also be dead) is what let a throttle-poisoned pool
			// wedge the whole client until a restart.
			c, err = x.dial(timeout)
		}
		if err != nil {
			lastErr = err
			continue
		}
		st, hdr, b, err := x.roundtrip(c, method, path, body, timeout)
		c.Close() // one request per connection (quota)
		if err == nil {
			return st, hdr, b, nil
		}
		lastErr = err
	}
	return 0, nil, nil, lastErr
}

func (x *xport) roundtrip(c *tls.Conn, method, path string, body []byte, timeout time.Duration) (int, http.Header, []byte, error) {
	c.SetDeadline(time.Now().Add(timeout))
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nContent-Length: %d\r\n\r\n", method, path, x.hostHdr, len(body))
	buf.Write(body)
	if _, err := c.Write(buf.Bytes()); err != nil {
		return 0, nil, nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b, err
}

// open establishes a session, retrying on transient connection failures — a
// dropped/timed-out connection is expected under the throttle, so we just try
// again on a fresh one rather than failing the whole stream.
func (x *xport) open(host string, port int) (string, error) {
	target := fmt.Sprintf("%s:%d", host, port)
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		st, _, b, err := x.do("POST", "/t/o", []byte(target), ctrlTimeout)
		if err == nil && st == 200 {
			return string(b), nil
		}
		if err == nil && st == 502 {
			return "", fmt.Errorf("target unreachable") // real error: don't retry
		}
		lastErr = err
		time.Sleep(time.Duration(80*(attempt+1)) * time.Millisecond)
	}
	return "", fmt.Errorf("open failed after retries: %v", lastErr)
}

type tunnel struct {
	x       *xport
	sid     string
	conn    net.Conn
	workers int
	nextW   int64 // atomic: lowest chunk still needed (ack)
	dead    int32
}

type fres struct {
	data  []byte
	eof   bool
	gone  bool
	empty bool // not yet produced (only the needed chunk long-polls; caller retries)
	prod  int  // producer's chunk count, for bounding prefetch
}

var debug bool

func (t *tunnel) closed() bool { return atomic.LoadInt32(&t.dead) != 0 }
func (t *tunnel) kill()        { atomic.StoreInt32(&t.dead, 1) }

// fetch does ONE request for downstream chunk i. wait=true long-polls (used only
// for the chunk the writer needs next); wait=false returns instantly (prefetch of
// already-produced chunks) so it never ties up a connection.
func (t *tunnel) fetch(i int, wait bool) fres {
	w, to := "0", fetchTimeout
	if wait {
		w, to = "1", pollTimeout
	}
	// Retry on a fresh connection: a dropped/slow connection (the throttle kills a
	// fraction of them) must not stall the in-order writer — a produced chunk comes
	// back in ~0.25s, so a >timeout attempt just means "connection dropped, retry".
	for att := 0; att < 15 && !t.closed(); att++ {
		ack := int(atomic.LoadInt64(&t.nextW))
		st, h, b, err := t.x.do("GET", fmt.Sprintf("/t/d?s=%s&i=%d&a=%d&w=%s", t.sid, i, ack, w), nil, to)
		if err != nil {
			continue // dropped connection — retry immediately on a fresh one
		}
		if st == 410 {
			return fres{gone: true}
		}
		if st != 200 {
			continue
		}
		prod, _ := strconv.Atoi(h.Get("X-Prod"))
		eof := h.Get("X-Eof") == "1"
		if len(b) > 0 || eof {
			return fres{data: b, eof: eof, prod: prod}
		}
		// Empty: for a long-poll the relay's hold expired (chunk still not produced)
		// — return so the writer can re-drive. For a prefetch this is a rare race.
		return fres{empty: true, prod: prod}
	}
	return fres{empty: true}
}

func (t *tunnel) upPump() {
	seq := 0
	buf := make([]byte, CHUNK)
	for !t.closed() {
		n, err := t.conn.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			ok := false
			for r := 0; r < 8 && !t.closed(); r++ {
				st, _, _, e := t.x.do("POST", fmt.Sprintf("/t/u?s=%s&i=%d", t.sid, seq), data, ctrlTimeout)
				if e == nil && st == 200 {
					ok = true
					break
				}
				if e == nil && st == 410 {
					t.kill()
					return
				}
				time.Sleep(120 * time.Millisecond)
			}
			if !ok {
				t.kill()
				return
			}
			seq++
		}
		if err != nil {
			// client half-closed its write side: tell the relay to CloseWrite the
			// target, but keep the download flowing until the target finishes.
			t.x.do("POST", fmt.Sprintf("/t/e?s=%s&i=%d", t.sid, seq), nil, ctrlTimeout)
			return
		}
	}
}

func (t *tunnel) downPump() {
	defer t.kill()
	var mu sync.Mutex
	pref := map[int]chan fres{} // in-flight prefetches of already-produced chunks
	prod := 0                   // known producer count (from X-Prod)
	prefetch := func(i int) {
		mu.Lock()
		if _, ok := pref[i]; ok {
			mu.Unlock()
			return
		}
		ch := make(chan fres, 1)
		pref[i] = ch
		mu.Unlock()
		go func() { ch <- t.fetch(i, false) }()
	}
	nextWrite := 0
	stuckSince := time.Now()
	for !t.closed() {
		if debug && time.Since(stuckSince) > 6*time.Second {
			log.Printf("STALL sid=%s nextWrite=%d prod=%d prefInflight=%d", t.sid, nextWrite, prod, len(pref))
			stuckSince = time.Now()
		}
		// Get the chunk we need next: use a prefetch result if we have one, else
		// fetch it directly with a long-poll (the ONLY blocking fetch per stream).
		mu.Lock()
		ch, have := pref[nextWrite]
		if have {
			delete(pref, nextWrite)
		}
		mu.Unlock()
		var r fres
		if have {
			r = <-ch
		} else {
			r = t.fetch(nextWrite, true)
		}
		if r.gone {
			return
		}
		if r.prod > prod {
			prod = r.prod
		}
		if r.empty {
			time.Sleep(15 * time.Millisecond) // needed chunk not ready yet; retry
			continue
		}
		if len(r.data) > 0 {
			if _, err := t.conn.Write(r.data); err != nil {
				return
			}
		}
		if r.eof {
			return
		}
		nextWrite++
		atomic.StoreInt64(&t.nextW, int64(nextWrite))
		stuckSince = time.Now()
		// Prefetch already-produced chunks within the window (never blocks).
		for i := nextWrite + 1; i <= nextWrite+t.workers && i < prod; i++ {
			prefetch(i)
		}
	}
}

func handleSocks(c net.Conn, x *xport, workers int) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	br := make([]byte, 2)
	if _, err := io.ReadFull(c, br); err != nil || br[0] != 5 {
		return
	}
	methods := make([]byte, br[1])
	io.ReadFull(c, methods)
	c.Write([]byte{5, 0}) // no auth
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil || (hdr[1] != 1 && hdr[1] != 3) {
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0}) // only CONNECT(1) and UDP ASSOCIATE(3)
		return
	}
	var host string
	switch hdr[3] {
	case 1:
		b := make([]byte, 4)
		io.ReadFull(c, b)
		host = net.IP(b).String()
	case 3:
		l := make([]byte, 1)
		io.ReadFull(c, l)
		b := make([]byte, l[0])
		io.ReadFull(c, b)
		host = string(b)
	case 4:
		b := make([]byte, 16)
		io.ReadFull(c, b)
		host = net.IP(b).String()
	default:
		return
	}
	pb := make([]byte, 2)
	io.ReadFull(c, pb)
	port := int(binary.BigEndian.Uint16(pb))
	c.SetDeadline(time.Time{})

	if hdr[1] == 3 { // UDP ASSOCIATE: the parsed host/port are the app's advertised source (ignored)
		handleUDPAssociate(c, x)
		return
	}

	sid, err := x.open(host, port)
	if err != nil {
		if debug {
			log.Printf("open %s:%d failed: %v", host, port, err)
		}
		c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	t := &tunnel{x: x, sid: sid, conn: c, workers: workers}
	go t.upPump()
	t.downPump() // returns on EOF or dead session
	t.kill()
	x.do("POST", "/t/c?s="+sid, nil, ctrlTimeout)
}

func runClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	url := fs.String("url", "https://vps.reasoners.org", "exit relay base URL")
	listen := fs.String("listen", "127.0.0.1:10900", "local SOCKS5 listen address")
	workers := fs.Int("workers", 16, "download prefetch window per stream")
	maxConns := fs.Int("maxconns", 96, "global cap on concurrent connections (all streams)")
	poolSize := fs.Int("pool", 64, "pre-warmed connection pool size (memory vs latency tradeoff)")
	chunk := fs.Int("chunk", CHUNK, "bytes per request (must stay under the ~16KB per-connection quota)")
	ctrlTO := fs.Duration("ctrl-timeout", ctrlTimeout, "timeout for open/upload/close")
	fetchTO := fs.Duration("fetch-timeout", fetchTimeout, "timeout for prefetch of a produced chunk")
	pollTO := fs.Duration("poll-timeout", pollTimeout, "timeout for the long-poll of the needed chunk")
	insecure := fs.Bool("insecure", false, "skip TLS cert verification")
	sni := fs.String("sni", "", "TLS server name (default: host from -url)")
	ubatch := fs.Duration("ubatch", udpBatchWindow, "UDP: coalesce outgoing datagrams for this long per send")
	upollers := fs.Int("upollers", udpPollers, "UDP: concurrent return-direction long-polls per association")
	blockQUIC := fs.Bool("block-quic", udpBlockQUIC, "UDP: drop QUIC (:443) so video/web uses the TCP path")
	dbg := fs.Bool("debug", false, "log per-stream failures")
	fs.Parse(args)

	host := strings.TrimPrefix(strings.TrimPrefix(*url, "https://"), "http://")
	host = strings.SplitN(host, "/", 2)[0]
	if *sni == "" {
		*sni = host
	}
	if p := os.Getenv("CPUPROFILE"); p != "" {
		f, _ := os.Create(p)
		pprof.StartCPUProfile(f)
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		go func() { <-ch; pprof.StopCPUProfile(); f.Close(); log.Printf("cpu profile -> %s", p); os.Exit(0) }()
		log.Printf("CPU profiling to %s", p)
	}
	debug = *dbg
	CHUNK, ctrlTimeout, fetchTimeout, pollTimeout = *chunk, *ctrlTO, *fetchTO, *pollTO
	udpBatchWindow, udpPollers, udpBlockQUIC = *ubatch, *upollers, *blockQUIC
	x := newXport(*url, *insecure, *sni, *maxConns, *poolSize)
	if st, _, _, err := x.do("GET", "/t/ping", nil, ctrlTimeout); err != nil || st != 200 {
		log.Fatalf("relay unreachable at %s: st=%d err=%v", *url, st, err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("cyclevpn client: SOCKS5 %s -> %s (workers=%d, TLS-resume on)", *listen, *url, *workers)
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleSocks(c, x, *workers)
	}
}
