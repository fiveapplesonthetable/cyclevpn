package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// A relay session holds the live TCP connection to the real destination and
// buffers both directions so the client can drive them over many short requests.
type session struct {
	conn net.Conn

	// downstream (dest -> client): indexed chunks, dropped once the client acks
	mu       sync.Mutex
	down     map[int][]byte
	produced int
	downEOF  bool

	// upstream (client -> dest): reassembled in order
	upMu   sync.Mutex
	upNext int
	upBuf  map[int][]byte
	upEnd  int // seq at which the client half-closed its write side (-1 = open)

	last int64
}

func newSession(c net.Conn) *session {
	s := &session{conn: c, down: map[int][]byte{}, upBuf: map[int][]byte{}, upEnd: -1, last: time.Now().Unix()}
	go s.reader()
	return s
}

func (s *session) reader() {
	buf := make([]byte, CHUNK)
	for {
		n, err := s.conn.Read(buf)
		s.mu.Lock()
		if n > 0 {
			b := make([]byte, n)
			copy(b, buf[:n])
			s.down[s.produced] = b
			s.produced++
			s.last = time.Now().Unix()
		}
		if err != nil {
			s.downEOF = true
			s.mu.Unlock()
			break
		}
		s.mu.Unlock()
	}
	s.conn.Close()
}

// readDown returns chunk i (blocking until produced or EOF), the current produced
// count, and eof. Drops any chunk below ack (already written by the client).
// Returns empty+!eof only if the wait deadline expires (client retries).
func (s *session) readDown(i, ack int) (data []byte, eof bool, prod int) {
	deadline := time.Now().Add(holdTime)
	for {
		s.mu.Lock()
		for k := range s.down {
			if k < ack {
				delete(s.down, k)
			}
		}
		prod = s.produced
		if b, ok := s.down[i]; ok {
			e := s.downEOF && i+1 >= s.produced
			s.last = time.Now().Unix()
			s.mu.Unlock()
			return b, e, prod
		}
		if s.downEOF {
			s.mu.Unlock()
			return nil, true, prod
		}
		s.last = time.Now().Unix()
		s.mu.Unlock()
		if time.Now().After(deadline) {
			return nil, false, prod
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// peekDown returns chunk i if it is already produced, without blocking.
func (s *session) peekDown(i, ack int) (data []byte, eof bool, prod int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.down {
		if k < ack {
			delete(s.down, k)
		}
	}
	s.last = time.Now().Unix()
	prod = s.produced
	if b, ok := s.down[i]; ok {
		return b, s.downEOF && i+1 >= s.produced, prod
	}
	return nil, s.downEOF, prod
}

func (s *session) writeUp(i int, data []byte) {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	if i >= s.upNext { // ignore duplicates from lost-ACK retries
		s.upBuf[i] = data
	}
	for {
		d, ok := s.upBuf[s.upNext]
		if !ok {
			break
		}
		if _, err := s.conn.Write(d); err != nil {
			break
		}
		delete(s.upBuf, s.upNext)
		s.upNext++
	}
	s.maybeCloseWriteLocked()
	s.last = time.Now().Unix()
}

// endUp records that the client half-closed after seq `end`; once all upstream
// chunks are written, the target's write side is closed (proper TCP half-close).
func (s *session) endUp(end int) {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	s.upEnd = end
	s.maybeCloseWriteLocked()
	s.last = time.Now().Unix()
}

func (s *session) maybeCloseWriteLocked() {
	if s.upEnd >= 0 && s.upNext >= s.upEnd {
		if tc, ok := s.conn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}
}

func (s *session) close() { s.conn.Close() }

type relay struct {
	mu   sync.Mutex
	sess map[string]*session
}

func (r *relay) reap() {
	for {
		time.Sleep(30 * time.Second)
		now := time.Now().Unix()
		r.mu.Lock()
		for k, s := range r.sess {
			if now-s.last > 120 {
				s.close()
				delete(r.sess, k)
			}
		}
		r.mu.Unlock()
	}
}

func (r *relay) get(sid string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sess[sid]
}

func (r *relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/t/o":
		body, _ := io.ReadAll(io.LimitReader(req.Body, 512))
		conn, err := net.DialTimeout("tcp", string(body), 15*time.Second)
		if err != nil {
			http.Error(w, "dial", 502)
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}
		var b [9]byte
		rand.Read(b[:])
		sid := hex.EncodeToString(b[:])
		r.mu.Lock()
		r.sess[sid] = newSession(conn)
		r.mu.Unlock()
		w.Write([]byte(sid))
	case "/t/u":
		q := req.URL.Query()
		s := r.get(q.Get("s"))
		if s == nil {
			http.Error(w, "gone", 410)
			return
		}
		i, _ := strconv.Atoi(q.Get("i"))
		body, _ := io.ReadAll(io.LimitReader(req.Body, int64(CHUNK*2)))
		s.writeUp(i, body)
		w.Write([]byte("ok"))
	case "/t/e":
		q := req.URL.Query()
		s := r.get(q.Get("s"))
		if s == nil {
			http.Error(w, "gone", 410)
			return
		}
		i, _ := strconv.Atoi(q.Get("i"))
		s.endUp(i)
		w.Write([]byte("ok"))
	case "/t/d":
		q := req.URL.Query()
		s := r.get(q.Get("s"))
		if s == nil {
			http.Error(w, "gone", 410)
			return
		}
		i, _ := strconv.Atoi(q.Get("i"))
		ack, _ := strconv.Atoi(q.Get("a"))
		wait := q.Get("w") == "1"
		var data []byte
		var eof bool
		var prod int
		if wait {
			data, eof, prod = s.readDown(i, ack) // long-poll: only for the needed chunk
		} else {
			data, eof, prod = s.peekDown(i, ack) // instant: for prefetching produced chunks
		}
		w.Header().Set("X-Prod", strconv.Itoa(prod))
		if eof {
			w.Header().Set("X-Eof", "1")
		}
		w.Write(data)
	case "/t/c":
		sid := req.URL.Query().Get("s")
		r.mu.Lock()
		if s := r.sess[sid]; s != nil {
			s.close()
			delete(r.sess, sid)
		}
		r.mu.Unlock()
		w.Write([]byte("ok"))
	case "/t/stat":
		s := r.get(req.URL.Query().Get("s"))
		if s == nil {
			http.Error(w, "gone", 410)
			return
		}
		s.mu.Lock()
		prod, eof, dn := s.produced, s.downEOF, len(s.down)
		s.mu.Unlock()
		s.upMu.Lock()
		un, ue, ub := s.upNext, s.upEnd, len(s.upBuf)
		s.upMu.Unlock()
		fmt.Fprintf(w, "produced=%d downEOF=%v downBuf=%d upNext=%d upEnd=%d upBuf=%d", prod, eof, dn, un, ue, ub)
	case "/t/ping":
		w.Write([]byte("pong"))
	default:
		http.Error(w, "?", 404)
	}
}

var holdTime = 4 * time.Second

func runRelay(args []string) {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8791", "listen address (put a TLS reverse proxy in front)")
	chunk := fs.Int("chunk", CHUNK, "bytes per chunk (match the client)")
	hold := fs.Duration("hold", holdTime, "how long a long-poll waits for the next chunk")
	fs.Parse(args)
	CHUNK, holdTime = *chunk, *hold
	r := &relay{sess: map[string]*session{}}
	go r.reap()
	srv := &http.Server{Addr: *listen, Handler: r}
	log.Printf("cyclevpn relay on %s", *listen)
	log.Fatal(srv.ListenAndServe())
}
