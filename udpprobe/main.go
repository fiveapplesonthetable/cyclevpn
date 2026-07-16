// udpprobe — emulate a voice call through a SOCKS5 UDP proxy: send datagrams at a
// fixed pps/size to an echo server, measure RTT, jitter, loss, and survival over time.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"
)

func main() {
	socks := flag.String("socks", "127.0.0.1:10905", "SOCKS5 proxy")
	dest := flag.String("dest", "", "echo server ip:port (through the tunnel)")
	pps := flag.Int("pps", 50, "datagrams per second")
	sz := flag.Int("bytes", 120, "payload bytes")
	secs := flag.Int("secs", 60, "duration")
	flag.Parse()

	dh, dp, _ := net.SplitHostPort(*dest)
	dip := net.ParseIP(dh).To4()
	dport := 0
	fmt.Sscanf(dp, "%d", &dport)

	// SOCKS5 UDP ASSOCIATE
	ctrl, err := net.Dial("tcp", *socks)
	if err != nil {
		fmt.Println("dial socks:", err)
		return
	}
	defer ctrl.Close()
	ctrl.Write([]byte{5, 1, 0})
	rb := make([]byte, 2)
	io.ReadFull(ctrl, rb)
	// UDP ASSOCIATE request, addr 0.0.0.0:0
	ctrl.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	rep := make([]byte, 10)
	if _, err := io.ReadFull(ctrl, rep); err != nil || rep[1] != 0 {
		fmt.Println("associate failed:", err, rep)
		return
	}
	bndPort := int(binary.BigEndian.Uint16(rep[8:10]))
	relayAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: bndPort}

	uc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		fmt.Println("listen udp:", err)
		return
	}
	defer uc.Close()

	hdr := []byte{0, 0, 0, 1, dip[0], dip[1], dip[2], dip[3], byte(dport >> 8), byte(dport)}

	var mu sync.Mutex
	sendTimes := map[uint64]time.Time{}
	var rtts []float64
	recvByBucket := map[int]int{}
	sentByBucket := map[int]int{}
	start := time.Now()
	done := make(chan struct{})

	// receiver
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := uc.Read(buf)
			if err != nil {
				return
			}
			// strip SOCKS UDP header (assume IPv4: 10 bytes)
			if n < 10+16 {
				continue
			}
			p := buf[10:n]
			seq := binary.BigEndian.Uint64(p[0:8])
			mu.Lock()
			st, ok := sendTimes[seq]
			if ok {
				rtt := time.Since(st).Seconds() * 1000
				rtts = append(rtts, rtt)
				recvByBucket[int(st.Sub(start).Seconds())/5]++
				delete(sendTimes, seq)
			}
			mu.Unlock()
		}
	}()

	// sender
	go func() {
		tick := time.NewTicker(time.Second / time.Duration(*pps))
		defer tick.Stop()
		var seq uint64
		payload := make([]byte, *sz)
		for range tick.C {
			select {
			case <-done:
				return
			default:
			}
			now := time.Now()
			binary.BigEndian.PutUint64(payload[0:8], seq)
			binary.BigEndian.PutUint64(payload[8:16], uint64(now.UnixNano()))
			pkt := append(append([]byte{}, hdr...), payload...)
			mu.Lock()
			sendTimes[seq] = now
			sentByBucket[int(now.Sub(start).Seconds())/5]++
			mu.Unlock()
			uc.WriteToUDP(pkt, relayAddr)
			seq++
		}
	}()

	time.Sleep(time.Duration(*secs) * time.Second)
	close(done)
	time.Sleep(600 * time.Millisecond) // let stragglers arrive
	uc.Close()

	mu.Lock()
	defer mu.Unlock()
	sent := 0
	for _, v := range sentByBucket {
		sent += v
	}
	recv := len(rtts)
	fmt.Printf("\n=== voice probe: %d pps, %d-byte payloads, %ds, dest=%s ===\n", *pps, *sz, *secs, *dest)
	fmt.Printf("sent=%d  recv=%d  loss=%.1f%%\n", sent, recv, 100*float64(sent-recv)/float64(max(sent, 1)))
	if recv > 0 {
		sort.Float64s(rtts)
		p := func(q float64) float64 { return rtts[int(q*float64(len(rtts)-1))] }
		var sum float64
		for _, r := range rtts {
			sum += r
		}
		fmt.Printf("RTT ms: min=%.0f  p50=%.0f  avg=%.0f  p95=%.0f  p99=%.0f  max=%.0f\n",
			rtts[0], p(0.5), sum/float64(recv), p(0.95), p(0.99), rtts[len(rtts)-1])
	}
	fmt.Printf("\nsurvival (per 5s bucket): sent -> recv  (loss%%)\n")
	nb := (*secs + 4) / 5
	for b := 0; b < nb; b++ {
		s, r := sentByBucket[b], recvByBucket[b]
		fmt.Printf("  %2ds-%2ds:  %3d -> %3d  (%.0f%%)\n", b*5, b*5+5, s, r, 100*float64(s-r)/float64(max(s, 1)))
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
