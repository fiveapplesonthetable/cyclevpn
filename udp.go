package main

// UDP support for low-bitrate real-time flows (voice calls). TCP downloads use the
// chunked/in-order path in client.go/relay.go; UDP is different: datagrams are
// independent, order doesn't matter, and latency matters more than throughput. So the
// UDP path optimizes for latency — small batched sends, always-waiting short long-polls
// for the return direction — while still riding the same pooled, TLS-resumed, sub-quota
// cycled connections that beat the throttle.

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var (
	udpHold        = 200 * time.Millisecond // relay: how long /u/r waits for a datagram before returning empty
	udpBatchWindow = 30 * time.Millisecond  // client: coalesce upstream datagrams for this long before one send
	udpPollers     = 3                      // client: concurrent return-direction long-polls kept in flight
	udpSendTO      = 400 * time.Millisecond // client: timeout for an upstream send (fail fast; a late voice packet is useless)
	udpRecvTO      = 350 * time.Millisecond // client: timeout for a return long-poll (must exceed udpHold; abandoning loses no data)
)

// ---- datagram framing: atyp(1) | addr | port(2 BE) | len(2 BE) | payload, repeated ----
// atyp: 1=IPv4 (4 bytes), 4=IPv6 (16 bytes), 3=domain (1 len byte + n)

func appendFrame(b []byte, atyp byte, addr []byte, port int, payload []byte) []byte {
	b = append(b, atyp)
	if atyp == 3 {
		b = append(b, byte(len(addr)))
	}
	b = append(b, addr...)
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(port))
	b = append(b, p[:]...)
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(payload)))
	b = append(b, l[:]...)
	return append(b, payload...)
}

func parseFrames(b []byte, fn func(atyp byte, addr []byte, port int, payload []byte)) {
	i := 0
	for i < len(b) {
		atyp := b[i]
		i++
		var addr []byte
		switch atyp {
		case 1:
			if i+4 > len(b) {
				return
			}
			addr = b[i : i+4]
			i += 4
		case 4:
			if i+16 > len(b) {
				return
			}
			addr = b[i : i+16]
			i += 16
		case 3:
			if i+1 > len(b) {
				return
			}
			n := int(b[i])
			i++
			if i+n > len(b) {
				return
			}
			addr = b[i : i+n]
			i += n
		default:
			return
		}
		if i+4 > len(b) {
			return
		}
		port := int(binary.BigEndian.Uint16(b[i : i+2]))
		pl := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
		i += 4
		if i+pl > len(b) {
			return
		}
		fn(atyp, addr, port, b[i:i+pl])
		i += pl
	}
}

// ---- SOCKS5 UDP request wrapping (RFC 1928): RSV(2) FRAG(1) ATYP(1) ADDR PORT(2) DATA ----

func parseSocksUDP(p []byte) (atyp byte, addr []byte, port int, payload []byte, ok bool) {
	if len(p) < 4 || p[2] != 0 { // no fragmentation
		return
	}
	atyp = p[3]
	i := 4
	switch atyp {
	case 1:
		if len(p) < i+4 {
			return
		}
		addr = p[i : i+4]
		i += 4
	case 4:
		if len(p) < i+16 {
			return
		}
		addr = p[i : i+16]
		i += 16
	case 3:
		if len(p) < i+1 {
			return
		}
		n := int(p[i])
		i++
		if len(p) < i+n {
			return
		}
		addr = p[i : i+n]
		i += n
	default:
		return
	}
	if len(p) < i+2 {
		return
	}
	port = int(binary.BigEndian.Uint16(p[i : i+2]))
	return atyp, addr, port, p[i+2:], true
}

func buildSocksUDP(atyp byte, addr []byte, port int, payload []byte) []byte {
	b := []byte{0, 0, 0, atyp}
	if atyp == 3 {
		b = append(b, byte(len(addr)))
	}
	b = append(b, addr...)
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], uint16(port))
	b = append(b, pb[:]...)
	return append(b, payload...)
}

// ---------------- relay side ----------------

// A UDP session holds one unconnected UDP socket. It can send to any destination and
// buffers whatever comes back (tagged with source) for the client to long-poll.
type udpSession struct {
	conn   *net.UDPConn
	mu     sync.Mutex
	inbox  []byte
	signal chan struct{}
	last   int64
}

func newUDPSession() (*udpSession, error) {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	u := &udpSession{conn: c, signal: make(chan struct{}, 1), last: time.Now().Unix()}
	go u.reader()
	return u, nil
}

func (u *udpSession) reader() {
	buf := make([]byte, 2048)
	for {
		n, src, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		atyp, ip := byte(1), src.IP.To4()
		if ip == nil {
			atyp, ip = 4, src.IP.To16()
		}
		u.mu.Lock()
		if len(u.inbox) < 256*1024 { // bound memory if the client stalls
			u.inbox = appendFrame(u.inbox, atyp, ip, src.Port, buf[:n])
		}
		u.last = time.Now().Unix()
		u.mu.Unlock()
		select {
		case u.signal <- struct{}{}:
		default:
		}
	}
}

// drain returns buffered datagrams, waiting up to hold for the first one. It wakes
// immediately when a datagram arrives, so a longer hold costs nothing on latency — it
// only makes idle polls (no traffic) return less often, saving connections.
func (u *udpSession) drain(hold time.Duration) []byte {
	deadline := time.Now().Add(hold)
	for {
		u.mu.Lock()
		if len(u.inbox) > 0 {
			out := u.inbox
			u.inbox = nil
			u.last = time.Now().Unix()
			u.mu.Unlock()
			return out
		}
		u.mu.Unlock()
		rem := time.Until(deadline)
		if rem <= 0 {
			return nil
		}
		select {
		case <-u.signal:
		case <-time.After(rem):
		}
	}
}

func (u *udpSession) send(frames []byte) {
	parseFrames(frames, func(atyp byte, addr []byte, port int, payload []byte) {
		var dst *net.UDPAddr
		switch atyp {
		case 1, 4:
			dst = &net.UDPAddr{IP: net.IP(addr), Port: port}
		case 3: // resolve at the exit — no DNS in Russia
			a, err := net.ResolveUDPAddr("udp", net.JoinHostPort(string(addr), strconv.Itoa(port)))
			if err != nil {
				return
			}
			dst = a
		}
		u.conn.WriteToUDP(payload, dst)
	})
	u.mu.Lock()
	u.last = time.Now().Unix()
	u.mu.Unlock()
}

func (u *udpSession) close() { u.conn.Close() }

func (r *relay) uGet(sid string) *udpSession {
	r.umu.Lock()
	defer r.umu.Unlock()
	return r.usess[sid]
}

func (r *relay) serveUDP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/u/o":
		u, err := newUDPSession()
		if err != nil {
			http.Error(w, "udp", 502)
			return
		}
		var b [9]byte
		rand.Read(b[:])
		sid := hex.EncodeToString(b[:])
		r.umu.Lock()
		r.usess[sid] = u
		r.umu.Unlock()
		w.Write([]byte(sid))
	case "/u/s":
		u := r.uGet(req.URL.Query().Get("s"))
		if u == nil {
			http.Error(w, "gone", 410)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(req.Body, 65536))
		u.send(body)
		w.Write([]byte("ok"))
	case "/u/r":
		u := r.uGet(req.URL.Query().Get("s"))
		if u == nil {
			http.Error(w, "gone", 410)
			return
		}
		w.Write(u.drain(udpHold))
	case "/u/c":
		sid := req.URL.Query().Get("s")
		r.umu.Lock()
		if u := r.usess[sid]; u != nil {
			u.close()
			delete(r.usess, sid)
		}
		r.umu.Unlock()
		w.Write([]byte("ok"))
	}
}

// ---------------- client side ----------------

func (x *xport) udpOpen() (string, error) {
	st, _, b, err := x.do("POST", "/u/o", nil, ctrlTimeout)
	if err != nil || st != 200 {
		return "", fmt.Errorf("udp open: st=%d err=%v", st, err)
	}
	return string(b), nil
}

// once does one request tuned for real-time UDP. It retries only on a *fast* connection
// error — a pooled connection the throttle silently killed fails in milliseconds, and
// skipping past it (with a fresh dial) is what keeps voice working on a churned box. It
// does NOT retry on a *timeout*: a request that ran the full deadline is abandoned,
// because retrying a slow request is what produces multi-second latency tails (a return
// long-poll loses no data when abandoned — the relay holds the datagrams; a send drops
// at most one datagram, which voice conceals).
func (x *xport) once(method, path string, body []byte, timeout time.Duration) (int, []byte, error) {
	x.sem <- struct{}{}
	defer func() { <-x.sem }()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var c *tls.Conn
		var err error
		if attempt == 0 {
			c, err = x.getConn(timeout) // try a pre-warmed connection first
		} else {
			c, err = x.dial(timeout) // pooled one was dead — dial fresh
		}
		if err != nil {
			lastErr = err
			continue
		}
		start := time.Now()
		st, _, b, err := x.roundtrip(c, method, path, body, timeout)
		c.Close()
		if err == nil {
			return st, b, nil
		}
		lastErr = err
		if time.Since(start) >= timeout { // genuine timeout, not a dead connection — don't chase it
			break
		}
	}
	return 0, nil, lastErr
}

func (x *xport) udpSend(sid string, frames []byte) { x.once("POST", "/u/s?s="+sid, frames, udpSendTO) }
func (x *xport) udpRecv(sid string) ([]byte, int) {
	st, b, err := x.once("GET", "/u/r?s="+sid, nil, udpRecvTO)
	if err != nil {
		return nil, -1
	}
	return b, st
}
func (x *xport) udpClose(sid string) { x.do("POST", "/u/c?s="+sid, nil, ctrlTimeout) }

// handleUDPAssociate serves a SOCKS5 UDP ASSOCIATE. The app sends SOCKS-wrapped UDP
// datagrams to the local relay socket we open; we tunnel them to the exit and stream
// replies back. The association lives as long as the control TCP connection is open.
func handleUDPAssociate(ctrl net.Conn, x *xport) {
	lc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		ctrl.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer lc.Close()
	la := lc.LocalAddr().(*net.UDPAddr)
	reply := []byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}
	binary.BigEndian.PutUint16(reply[8:], uint16(la.Port))
	ctrl.Write(reply)

	sid, err := x.udpOpen()
	if err != nil {
		if debug {
			log.Printf("udp open failed: %v", err)
		}
		return
	}
	defer x.udpClose(sid)
	if debug {
		log.Printf("udp associate: sid=%s local=%s", sid, la)
	}

	var appAddr *net.UDPAddr
	var appMu sync.Mutex
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }
	defer stop()

	// return direction: keep pollers in flight so a reply is delivered ~0.5 RTT after it lands.
	for i := 0; i < udpPollers; i++ {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				data, st := x.udpRecv(sid)
				if st == 410 {
					stop()
					return
				}
				if len(data) == 0 {
					continue
				}
				appMu.Lock()
				dst := appAddr
				appMu.Unlock()
				if debug {
					log.Printf("udp recv: %d bytes from relay, appAddr=%v", len(data), dst)
				}
				if dst == nil {
					continue
				}
				parseFrames(data, func(atyp byte, addr []byte, port int, payload []byte) {
					lc.WriteToUDP(buildSocksUDP(atyp, addr, port, payload), dst)
				})
			}
		}()
	}

	// send direction: read app datagrams, coalesce for up to udpBatchWindow, tunnel out.
	go func() {
		buf := make([]byte, 2048)
		var batch []byte
		var start time.Time
		flush := func() {
			if len(batch) == 0 {
				return
			}
			frames := batch
			batch = nil
			start = time.Time{}
			go x.udpSend(sid, frames)
		}
		for {
			lc.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
			n, src, err := lc.ReadFromUDP(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					if !start.IsZero() && time.Since(start) >= udpBatchWindow {
						flush()
					}
					select {
					case <-done:
						return
					default:
						continue
					}
				}
				stop()
				return
			}
			appMu.Lock()
			first := appAddr == nil
			if first {
				appAddr = src
			}
			appMu.Unlock()
			if debug && first {
				log.Printf("udp send: first app packet from %s, %d bytes", src, n)
			}
			if atyp, addr, port, payload, ok := parseSocksUDP(buf[:n]); ok {
				batch = appendFrame(batch, atyp, addr, port, payload)
				if start.IsZero() {
					start = time.Now()
				}
				if time.Since(start) >= udpBatchWindow || len(batch) > 4000 {
					flush()
				}
			}
		}
	}()

	// hold the association open until the control connection closes.
	tmp := make([]byte, 256)
	for {
		ctrl.SetReadDeadline(time.Now().Add(600 * time.Second))
		if _, err := ctrl.Read(tmp); err != nil {
			return
		}
	}
}
