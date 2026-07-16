package main

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type xport struct {
	base string
	hc   *http.Client
}

// newXport builds an HTTP client where every request is a FRESH TCP connection
// (so each stays under the per-connection quota) but the TLS handshake is
// RESUMED from a shared session cache — cheap on a 1-CPU box.
func newXport(base string, insecure bool, serverName string) *xport {
	tr := &http.Transport{
		DisableKeepAlives:   true, // one TCP connection per request
		MaxIdleConns:        0,
		MaxConnsPerHost:     0,
		TLSHandshakeTimeout: 12 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: insecure,
			ServerName:         serverName,
			ClientSessionCache: tls.NewLRUClientSessionCache(1024),
			MinVersion:         tls.VersionTLS12,
		},
	}
	return &xport{base: strings.TrimRight(base, "/"), hc: &http.Client{Transport: tr, Timeout: 25 * time.Second}}
}

func (x *xport) do(method, path string, body []byte) (int, http.Header, []byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, x.base+path, r)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Connection", "close")
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	resp, err := x.hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b, err
}

func (x *xport) open(host string, port int) (string, error) {
	st, _, b, err := x.do("POST", "/t/o", []byte(fmt.Sprintf("%s:%d", host, port)))
	if err != nil {
		return "", err
	}
	if st != 200 {
		return "", fmt.Errorf("open %d", st)
	}
	return string(b), nil
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
	data []byte
	eof  bool
	gone bool
}

func (t *tunnel) closed() bool { return atomic.LoadInt32(&t.dead) != 0 }
func (t *tunnel) kill()        { atomic.StoreInt32(&t.dead, 1) }

func (t *tunnel) fetch(i int) fres {
	ack := int(atomic.LoadInt64(&t.nextW))
	for attempt := 0; attempt < 6 && !t.closed(); attempt++ {
		st, h, b, err := t.x.do("GET", fmt.Sprintf("/t/d?s=%s&i=%d&a=%d", t.sid, i, ack), nil)
		if err == nil {
			if st == 410 {
				return fres{gone: true}
			}
			if st == 200 {
				return fres{data: b, eof: h.Get("X-Eof") == "1"}
			}
		}
		time.Sleep(120 * time.Millisecond)
	}
	return fres{} // empty, not eof -> retry same index
}

func (t *tunnel) upPump() {
	defer func() { t.x.do("POST", "/t/c?s="+t.sid, nil) }()
	seq := 0
	buf := make([]byte, CHUNK)
	for !t.closed() {
		n, err := t.conn.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			ok := false
			for r := 0; r < 5 && !t.closed(); r++ {
				st, _, _, e := t.x.do("POST", fmt.Sprintf("/t/u?s=%s&i=%d", t.sid, seq), data)
				if e == nil && st == 200 {
					ok = true
					break
				}
				if e == nil && st == 410 {
					t.kill()
					return
				}
				time.Sleep(150 * time.Millisecond)
			}
			if !ok {
				t.kill()
				return
			}
			seq++
		}
		if err != nil {
			return
		}
	}
}

func (t *tunnel) downPump() {
	defer t.kill()
	results := map[int]chan fres{}
	var mu sync.Mutex
	sem := make(chan struct{}, t.workers)
	issue := func(i int) {
		ch := make(chan fres, 1)
		mu.Lock()
		results[i] = ch
		mu.Unlock()
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			ch <- t.fetch(i)
		}()
	}
	for i := 0; i < t.workers; i++ {
		issue(i)
	}
	nextWrite := 0
	for !t.closed() {
		mu.Lock()
		ch := results[nextWrite]
		mu.Unlock()
		if ch == nil {
			issue(nextWrite)
			continue
		}
		r := <-ch
		mu.Lock()
		delete(results, nextWrite)
		mu.Unlock()
		if r.gone {
			return
		}
		if len(r.data) == 0 && !r.eof {
			issue(nextWrite) // not ready — re-request same index, do NOT advance
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
		issue(nextWrite + t.workers - 1) // keep the window full
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
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[1] != 1 {
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
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

	sid, err := x.open(host, port)
	if err != nil {
		c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	t := &tunnel{x: x, sid: sid, conn: c, workers: workers}
	go t.upPump()
	t.downPump()
}

func runClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	url := fs.String("url", "https://vps.reasoners.org", "exit relay base URL")
	listen := fs.String("listen", "127.0.0.1:10900", "local SOCKS5 listen address")
	workers := fs.Int("workers", 24, "parallel download connections per stream")
	insecure := fs.Bool("insecure", false, "skip TLS cert verification")
	sni := fs.String("sni", "", "TLS server name (default: host from -url)")
	fs.Parse(args)

	host := strings.TrimPrefix(strings.TrimPrefix(*url, "https://"), "http://")
	host = strings.SplitN(host, "/", 2)[0]
	if *sni == "" {
		*sni = host
	}
	x := newXport(*url, *insecure, *sni)
	if st, _, _, err := x.do("GET", "/t/ping", nil); err != nil || st != 200 {
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
