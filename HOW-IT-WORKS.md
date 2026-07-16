# How cyclevpn works

Companion to the README. The README covers deployment; this covers mechanism: how
Russia throttles, why standard tools fail against it, and how the two-program design
moves a full-speed TCP stream across the border by slicing it across many short
connections. Read top to bottom.

1. How Russia detects and throttles
2. Why standard circumvention tools fail
3. The core idea: a stream is not a connection
4. The machines and their roles
5. Protocol layers, end to end
6. One connection, start to finish
7. Upload path
8. Download path
9. Half-close
10. Long-lived streams and calls
11. What would make it faster
12. Why it runs on one CPU: pool + TLS resume
13. Reliability over lossy connections
14. Throughput ceiling
15. Code map

---

## 1. How Russia detects and throttles

Filtering runs on operator-mandated DPI appliances (TSPU) inside every ISP. They
inspect three layers:

- **IP.** Source/destination addresses. Datacenter and known-VPN ranges carry a
  reputation score; some are throttled regardless of payload.
- **TCP.** Ports, and per-connection behavior: how many connections, how fast they
  open, how many bytes each carries.
- **TLS.** The ClientHello carries the destination hostname in the **SNI** field in
  cleartext — it must be cleartext, since it precedes key negotiation. TSPU reads it
  on every connection. This is the primary key.

The throttle on a denylisted SNI, on the wire:

```
   ── SYN / SYN-ACK / ACK ─────────▶   TCP handshake completes, socket ESTABLISHED
   ── TLS ClientHello (SNI=name) ──▶   TSPU reads the cleartext SNI here
   ── ServerHello / Finished ──────▶   TLS completes
   ── GET /file ───────────────────▶
   ◀═ data … data … (≈16 KB) ══════    first ~16 KB at full speed
   ◀  (nothing)                        then every further packet is dropped
```

After ~16 KB on one connection, TSPU drops every subsequent packet in both
directions. It sends no RST and advertises no zero-window, so both kernels still see
`ESTABLISHED` with no error. The sender's kernel retransmits on RTO backoff (200ms,
400ms, 800ms…); every retransmit is also dropped; after ~15 tries the socket returns
`ETIMEDOUT`. The application sees a hang, then a dead connection.

Two measurements pin the mechanism:

- Same Cloudflare IP, SNI `speed.cloudflare.com`: 30 MB/s. SNI `gate.reasoners.org`:
  ~0.5 KB/s. The discriminator is the name, not the IP or the route.
- A second request on the same still-open connection returns 0 bytes. The quota is
  per-connection, not per-flow or per-second.

Constraint: **~16 KB per TCP connection to a denylisted SNI, enforced by silent drop,
keyed on cleartext SNI, with IP reputation as a coarse pre-filter.**

### Why this doesn't break the Russian internet

An obvious objection: if every connection dies after 16 KB, how does anything work?
How do Russians bank, stream, run businesses?

Because the quota is **not global**. It's a targeted instrument, triggered
selectively by one or more of:

- a **denylisted SNI** (VPN/circumvention/blocked hostnames),
- a **suspicious IP** (datacenter and known-VPN ranges — see the measured raw-TCP
  result in §2: even a no-SNI connection to a VPS IP got throttled), and/or
- a recognizable **protocol fingerprint** (WireGuard's fixed handshake, etc.).

The overwhelming majority of traffic matches none of these. Domestic Russian sites,
banks, government services, Yandex, VK, and the big whitelisted global CDNs
(Google, Cloudflare, Fastly, Apple) run at **full line speed** — no quota. That's the
country's working internet.

They *have* to leave it that way. A blanket 16 KB-per-connection cap would break every
website, every mobile app, every payment terminal, every business — and their own
state services. The economy runs on those connections. So the throttle can only ever
be a scalpel aimed at traffic that *looks* like circumvention, never a blanket cap.

That necessity is exactly the seam cyclevpn exploits. To slip through you need traffic
that (1) reveals no denylisted SNI, (2) rides an acceptable-looking TLS/CDN shape,
(3) doesn't match a known tunnel-protocol fingerprint, and (4) keeps each connection
under the quota. cyclevpn's transport is a stream of ordinary-looking short HTTPS
requests, which satisfies all four. The enforcement primitive — a per-connection byte
quota — **resets on every new connection**, and that reset is the whole game.

---

## 2. Why standard circumvention tools fail

Every off-the-shelf approach fails for one of three reasons: it presents a denylisted
SNI, it rides a whitelisted SNI it can't actually terminate traffic on, or it carries
bulk over a single connection that hits the 16 KB wall. What we tested:

| Technique | Why it fails here |
|---|---|
| **Plain VLESS / VMess / Trojan over TLS** | SNI is your own domain → denylisted → 16 KB quota on the one data connection. |
| **REALITY** (xtls-rprx-vision) | Borrows a real whitelisted SNI (dl.google.com), but data still flows over one long TCP connection, and the VPS source IP is reputation-throttled regardless of SNI. Bulk dies. |
| **Hysteria2 / TUAM / Brutal** | UDP/QUIC transport. Russia throttles/drops the UDP, and the VPS IP is penalized. Brutal's loss-tolerant congestion control can't help when packets are dropped wholesale. |
| **XHTTP / SplitHTTP** | Upload splits into many requests, but the **download is one long stream**. Tiny requests pass; the download hits the per-connection quota and stalls. Confirmed: works from a non-Russian vantage, 0 useful throughput from Russia. |
| **TLS fragmentation / byedpi / zapret desync** | Splits the ClientHello across segments so simple DPI can't parse the SNI. TSPU reassembles before matching. ~30 strategies plus a TTL sweep gave at best ~2×, still throttled. Effective against residential DPI, not this appliance. |
| **ECH** (Encrypted ClientHello) | Actually hides the SNI. Russia blocks handshakes that use ECH outright → 0 bytes. Works from the sandbox, dead from Russia. |
| **Domain fronting** | SNI = a big CDN name, `Host:` = your backend on the same CDN. Cloudflare returns 403 on cross-tenant fronting; it's been disabled. |
| **Platform hosting** (`*.workers.dev`, `*.pages.dev`, `*.run.app`, `*.vercel.app`, `*.fly.dev`, `*.deno.dev`) | All throttled: the platform apex names are denylisted or the pattern is flagged. |
| **Point traffic at a real whitelisted host** (speed.cloudflare.com etc.) | You don't control those servers, so you can't terminate a tunnel on them. The fast SNI is unusable as an endpoint. |
| **WireGuard / Tailscale / OpenVPN** | One tunnel = one flow. WireGuard is a single UDP flow (bulk UDP is dropped across the border — measured below); Tailscale's DERP fallback is a single TLS connection (per-connection quota). See the measured results below. |
| **Commercial VPNs** | Endpoint IP ranges are known and blocked/throttled at the IP layer. |

The common thread: the quota is per-connection, and none of these break bulk traffic
into enough separate connections. cyclevpn is the piece that does exactly that.

### Measured: WireGuard and Tailscale from Russia

We ran the tests rather than assume. All measured RU box (176.113.82.126) → foreign
box (Contabo 169.58.27.100), across the real border:

| Test | Result | Reading |
|---|---|---|
| WireGuard **handshake** (small packets) | completes, 50 ms RTT, 0% ping loss | tiny packets are under quota, so the tunnel *forms* |
| WireGuard **bulk** (iperf3 through tunnel) | connection times out, ~0 throughput | the tunnel carries no real traffic |
| Raw **UDP** flood, 100 Mbit target | sender pushed 90 Mbit/s, **receiver got 0 bytes** | bulk UDP is dropped wholesale in transit |
| Raw **TCP**, no TLS/SNI | ~1 Mbit/s for ~1 s, then socket killed | even without an SNI, the datacenter IP is throttled |
| **cyclevpn** (many short conns, same IP) | 7.1 Mbit/s sustained | per-connection quota resets per connection |

What this establishes:

- **WireGuard direct = dead for bulk.** The handshake and keepalives (small) pass, so
  the tunnel looks up — but the moment it carries a real transfer the UDP flow is
  dropped. The raw-UDP row is the proof: 90 Mbit sent, **zero received**.
- **Tailscale direct mode is WireGuard**, so it inherits that result: unusable.
- **Tailscale relay mode (DERP)** is a single long-lived TLS connection to a Tailscale
  relay — one connection carrying everything — which is exactly the single-connection
  case the ~16 KB quota kills. Dead too.
- **Tailscale feasibility verdict: non-viable, both modes.** It isn't cleanly
  *blocked* (control/handshake traffic connects), but there is no usable bulk path.
  A full end-to-end Tailscale login (needs an interactive browser auth) would only
  re-confirm a transport verdict already settled by the WireGuard and single-TLS
  measurements.
- The raw-TCP row adds a nuance to §1: throttling is **not purely SNI-based** — a
  datacenter/VPS IP is penalized even with no SNI at all. But note the last row:
  cyclevpn gets **7.1 Mbit/s to the very same IP** that kills a single TCP stream at
  ~1 Mbit/s. That's the entire thesis in one comparison — the quota is per-connection,
  so many small connections beat one big one even against a reputation-flagged IP.

The general rule this confirms: **any design with one persistent tunnel loses; only
spreading bulk across many sub-quota connections wins.** WireGuard, Tailscale,
OpenVPN, plain VLESS/REALITY all build one tunnel. cyclevpn refuses to.

---

## 3. The core idea: a stream is not a connection

An application's byte stream is data. It does not have to travel over one TCP
connection. If the bytes arrive in order, the application is satisfied and never
learns how they crossed.

The quota is per connection, so: cut the stream into ~14 KB chunks, send each chunk on
its own fresh short-lived connection (under the wall), discard the connection, run
many in parallel, reassemble in order on the far side. Call it connection cycling. The
rest of this document is the engineering that makes it reliable and fast on one CPU.

---

## 4. The machines and their roles

```
  PHONE                ENTRY box (in Russia)            EXIT box (outside Russia)        DESTINATION
  Shadowrocket   ─────▶ xray ──SOCKS──▶ cyclevpn client ══════▶ Caddy ──▶ cyclevpn relay ─────▶ example.com
  (stock VLESS)  TCP    (VLESS srv)     (SOCKS + cycler)  many  (TLS)     (holds real conn)  TCP
                domestic                                  short                              real
                (not throttled)                          HTTPS
```

**Phone.** Stock app, stock protocol (VLESS). No custom code — that's a hard
requirement, since shipping a custom client to iOS is impractical. Its only job is to
hand all traffic to the entry box.

**Entry box, inside Russia, two processes:**
- **xray** — a standard VLESS server. Terminates the phone's connection, authenticates
  by UUID, learns the target host:port, forwards the raw stream over local SOCKS5 to
  the cycler. Knows nothing about the throttle. It can speak a plain protocol because
  phone→entry is domestic (Russia→Russia), which TSPU does not apply the foreign-SNI
  quota to.
- **cyclevpn client** — the Russian-side logic. Accepts each SOCKS connection, opens a
  session on the relay, and moves the bytes across the border as many short HTTPS
  chunk-requests: cycling connections under the quota, retrying drops, reassembling
  order.

**Exit box, outside Russia, two processes:**
- **Caddy** — TLS reverse proxy. Presents a valid-cert HTTPS endpoint and forwards
  decrypted requests to the relay on localhost. Handles certificates so the relay
  speaks plain HTTP internally.
- **cyclevpn relay** — the foreign-side logic. Per session, holds the one real
  long-lived TCP connection to the destination, reads the download into numbered
  chunks, reassembles the upload in order, serves chunks to the client.

**Destination.** Sees one ordinary TCP connection from the exit's IP. Unaware of the
tunnel. The exit's IP is the apparent source.

Two hops exist so the phone can stay stock: cycling only happens between the two
custom programs, and the domestic hop bridges the stock phone into that path.

---

## 5. Protocol layers, end to end

On the throttled hop, one chunk of your request is wrapped like this:

```
  your bytes            "GET /page HTTP/1.1  Host: example.com"     ← application
  cyclevpn slice        one 14 KB chunk of that stream, tagged #i   ← our framing
  HTTP/1.1              GET /t/d?s=<sid>&i=<i>&a=<ack>&w=0           ← transport verb
  TLS                   encrypted; ClientHello SNI = vps.reasoners.org  ← what TSPU sees
  TCP                   fresh connection, carries one chunk then dies ← quota lives here
  IP                    src=entry, dst=exit                          ← reputation checked here
```

Sent directly, your request is denylisted (its SNI and single long stream). Re-wrapped,
it becomes a short HTTPS request to our relay whose visible SNI is the relay's name and
whose connection stays under quota. The real destination name is inside the encrypted
body, revealed only at the exit when the relay dials out.

End to end, the same byte is wrapped and unwrapped several times:

```
  PHONE            ENTRY xray         ENTRY client        EXIT relay          DEST
  VLESS/TLS/TCP ─▶ strip VLESS ─────▶ slice + wrap ─────▶ unwrap + reassemble ─▶ plain TCP
  (domestic)       raw over SOCKS5    HTTPS chunks         write to real conn
```

- **Phone→entry:** VLESS/TLS/TCP, domestic, unthrottled. xray strips VLESS.
- **xray→client:** raw stream over loopback SOCKS5.
- **client→relay:** the sliced, cycled hop — the only leg crossing the throttled
  border.
- **relay→dest:** relay writes reassembled bytes to the one real TCP connection.

Each layer understands only its own wrapper. TSPU reads only the outer wrappers on the
border hop, and those are deliberately unremarkable.

---

## 6. One connection, start to finish

Trace `https://example.com` opened through the tunnel.

**6a. Phone → entry (xray).** Shadowrocket opens one TCP connection to
`176.113.82.126:2053`, VLESS header says "reach example.com:443," then app bytes.
Domestic, so no wall. xray is in `LISTEN` on `:2053`; each SYN becomes an
`ESTABLISHED` socket via `accept()`. xray parses the header and forwards over SOCKS to
`127.0.0.1:10900`.

**6b. SOCKS5 handshake** (`handleSocks`, client.go:334):

```
xray → client:  05 01 00                          SOCKS5, method no-auth
client → xray:  05 00                              accepted
xray → client:  05 01 00 03 0B example.com 01BB    CONNECT example.com:443
client → xray:  05 00 00 01 00000000 0000          success
```

The client keeps the hostname as a string (client.go:349-370). No DNS on the entry
box — the exit resolves it, so DNS exits outside Russia (no DNS blocking, no leak).

**6c. Open a session** (`open` → `/t/o`). The client can't reach example.com from
Russia, so it posts the target to the relay:

```
POST /t/o   body: "example.com:443"
```

The relay dials the real `example.com:443` (unthrottled from the exit), wraps it in a
`session`, returns an 18-hex-char `sid`, and spawns `session.reader()` (relay.go:43):
a goroutine looping on `conn.Read()`, slicing the download into 14 KB chunks
`down[0], down[1], …` and incrementing `produced`. It pre-buffers the download so
chunks are ready before the client asks. The real connection to example.com lives here
and never cycles (§10).

**6d. Two directions.** TCP is full-duplex. `handleSocks` runs `upPump` (goroutine)
and `downPump` (main) concurrently (client.go:383-384).

---

## 7. Upload path (client → target)

`upPump` (client.go:233) reads app bytes in ≤14 KB chunks and posts each:
`POST /t/u?s=<sid>&i=<seq>`, body = chunk.

`writeUp(i, data)` (relay.go:114) enforces order, since posts race and can arrive out
of order. It holds `upNext` (next writable sequence) and buffer `upBuf`. A chunk ahead
of `upNext` is buffered; then it drains in order (`while upBuf[upNext] exists: write,
increment`). Bytes hit example.com in the exact order the app produced them.

Idempotent: if a post's response is lost, the client retries the same `i` on a fresh
connection (8×, client.go:242); the relay ignores a chunk it already wrote (`i >=
upNext`, relay.go:117). Number, dedupe on receive, retry on send.

---

## 8. Download path (target → client)

Throughput lives here: many chunks in flight (parallelism beats the quota) written to
the app strictly in order (TCP's contract). `downPump` (client.go:269) does two things.

**In-order writer.** `nextWrite` is the one chunk it must write next; it can't skip.
For that chunk it long-polls: `GET /t/d?s=<sid>&i=<nextWrite>&a=<ack>&w=1`. `w=1`
means wait; `readDown` (relay.go:68) blocks up to `holdTime` (4s) until the chunk
exists. This is the only blocking fetch per stream.

**Prefetchers.** While the writer is on N, chunks N+1… are usually already produced.
After each write, `downPump` fires non-blocking fetches for the next `workers` chunks
that exist (client.go:328): `GET /t/d?…&w=0`. `w=0` → `peekDown` (relay.go:98) returns
the chunk if produced, else empty immediately. These run in parallel, each on its own
fresh TLS connection, each carrying one 14 KB chunk under the quota. That fan-out is
the throughput. Results land in per-index channels; when the writer reaches N it
consumes the prefetch instantly instead of blocking.

**X-Prod.** Every `/t/d` response carries `X-Prod: <produced>` (relay.go:239). The
client prefetches only `i < prod` (client.go:328), so every connection is spent on a
chunk known to exist — no requests wasted, no connections parked waiting on data that
isn't there. On a one-CPU box with a bounded connection budget, that's the difference
between fast and not.

**Ack.** Each `/t/d` sends `a=nextWrite`. The relay deletes chunks below the ack
(relay.go:72), so it buffers only the in-flight window. Same idea as TCP advancing the
window on ack, at the app layer.

---

## 9. Half-close

TCP closes each direction independently. An HTTP client finishes sending its request
(closes its write side) but keeps reading for the response — `shutdown(fd, SHUT_WR)`,
a FIN on the write half only (Go: `TCPConn.CloseWrite()`).

When the app half-closes (`conn.Read` returns EOF), `upPump` posts
`/t/e?s=<sid>&i=<seq>` (client.go:263). The relay's `endUp` + `maybeCloseWriteLocked`
(relay.go:145) calls `CloseWrite()` on the target only after all upstream chunks below
`seq` are written, so the FIN reaches example.com after the last request byte, never
before, while the download keeps flowing. Servers that wait for a clean FIN before
responding would otherwise hang.

Teardown: at download EOF (`X-Eof: 1`, relay.go:79) `downPump` returns, `handleSocks`
kills the tunnel and posts `/t/c?s=<sid>` to free the session (client.go:386).

---

## 10. Long-lived streams and calls

If every connection dies after 16 KB, how does a 40-minute video or a persistent
websocket survive? Because the connection that must stay alive is not one of the
cycled ones.

```
  world A: the real connection            world B: the transport
  exit ──one long TCP──▶ youtube          entry ══ conn ══ conn ══ conn ══▶ exit
       (open for the whole video)              (each carries ~1 chunk, then dies)
```

The destination is in world A: one connection from the exit's IP, held open. It never
cycles, so the destination just sees a client that stays connected. World B is
stateless plumbing; the state (stream position, the live socket) lives at the
endpoints — the relay's session and the client's counters — not in any transport
connection. Losing a transport connection loses at most one chunk, renumbered and
retried (§13).

Video also buffers seconds ahead, absorbing the timing jitter from cycling, so
playback stays smooth on bursty delivery.

### Voice calls (UDP)

Real-time calls are UDP, and the naive expectation is that they can't work: a call's
UDP flow is a single flow, and we measured (§2) that a single UDP flow across the
border dies at the ~32 KB quota — for a voice call at ~3–4 KB/s that's ~8 seconds, then
silence. That is exactly the "call connects, works a few seconds, drops" symptom. And
going **direct** doesn't fix it, because Russia throttles the call servers the same way.

But the fix is the same principle as the rest of this document: **spread the call over
many sub-quota connections instead of one flow.** cyclevpn carries UDP (`udp.go`), and
because a voice call is *low-bitrate*, it cycles comfortably under the quota. Two things
make it work as a real-time path rather than a slow one:

- **Latency-first transport, not throughput-first.** The download path (§7) buffers and
  retries for throughput; the UDP path does the opposite. Outgoing datagrams are
  coalesced for only ~30 ms then sent; the return direction keeps a few short long-polls
  always waiting so a datagram is delivered ~½ RTT after it lands.
- **Retry on connection-error, not on timeout (`xport.once`).** This split is the
  crux. A *timed-out* request is abandoned, never chased — retrying a slow request is
  what creates multi-second latency tails (abandoning a return poll loses **no data**;
  the relay holds the datagrams for the next poll). But a *connection error* — a
  pooled connection the throttle silently killed, which fails in milliseconds — is
  retried immediately on a fresh dial. Skipping past dead pooled connections is what
  keeps voice working on a churned box; without it, a single dead connection in the
  pool drops the request (an early version failed exactly this way under load).

**Measured** (RU → exit, 50 pps voice-rate UDP through the tunnel to an echo server):

| Duration | Loss | RTT avg | RTT p99 | RTT max |
|---|---|---|---|---|
| 60 s (rested box) | **0.1%** | 79 ms | 114 ms | 183 ms |
| 120 s | 3.6% | 81 ms | 146 ms | 340 ms |
| video-rate (400 kbit/s) | ~7% | 83 ms | 134 ms | 206 ms |

Voice is call-quality: median RTT ~80 ms, jitter (p99−p50) ~30 ms, and it **survives
past the quota** where a single flow would have died at 8 s. As with everything here,
quality tracks box health — a box degraded by heavy churn shows higher loss (15%+),
same escalation as §13. **Video calls are marginal** (higher bitrate → more churn →
~7%+ loss); voice is the reliable target.

No configuration is needed — the client answers SOCKS5 `UDP ASSOCIATE` automatically, so
a phone that routes UDP through the tunnel (VLESS carries it) just works. Tunables:
`-ubatch` (send coalescing window), `-upollers` (return long-polls in flight), relay
`-uhold` (return long-poll hold).

Internals: the client's `handleUDPAssociate` opens a local UDP socket, tunnels each
datagram to a relay UDP session (`POST /u/s`), and streams replies back
(`GET /u/r`, short-hold long-poll). The relay session (`udpSession`) holds one
unconnected UDP socket, sends to the datagram's destination (resolving domains at the
exit), and buffers replies tagged with their source for the client to drain. Endpoints:
`/u/o` open, `/u/s` send, `/u/r` receive, `/u/c` close.

---

## 11. What would make it faster

`throughput ≈ 16 KB × connections-opened-per-second`. The 16 KB is fixed by the
throttle. The only lever is connection rate.

**Whitelisted SNI (largest, hardest).** The quota keys on the name; whitelisted CDN
names get no quota (30 MB/s vs 0.5 KB/s, same IP). If the transport could present a
whitelisted SNI and still reach our relay — domain fronting, or a relay behind a CDN
that trusts the name — throughput jumps by orders of magnitude and cycling becomes
nearly unnecessary. The obvious versions are defended (Cloudflare 403s fronting;
whitelisted names can't be terminated on). This changes the class of the problem, not
just the constant. Worth re-probing as CDN/TSPU configs drift.

**More CPU on the entry box.** Part of the ceiling is the core doing TLS handshakes.
More cores → more handshakes/sec → more connections in flight, up to where escalation
binds instead. Likely 2–3×, not 10×. RAM and network on the $3 box are already
adequate; CPU is the only upgrade that moves the number.

**Chunk size toward the wall.** 14 KB under a ~16 KB quota. ~15.5 KB is ~10% fewer
connections for the same bytes, so ~10% less churn and CPU. Safe if it stays under the
real quota with margin; if it doesn't, every connection dies. Measure, don't guess.

**Multiple exit relays.** If escalation keys partly on (entry box ↔ one destination
name), spreading transport across several exit hostnames could keep each under the
threshold. Unproven on this route.

**QUIC/HTTP-2 — dead end.** HTTP/2 multiplexes streams onto one connection, which is
exactly what the per-connection quota punishes. One fresh connection per chunk is the
shape the throttle rewards.

Measured non-improvements: `-workers`/`-pool` past the moderate default were slower
(§14); `-fetch-timeout 900ms` cut throughput to 1.6 Mbit/s by abandoning connections
that would have delivered. And sustained benchmarking trips escalation (7 → 0.6
Mbit/s), so you can't measure your way up.

---

## 12. Why it runs on one CPU: pool + TLS resume

We open many connections. Each new HTTPS connection costs a TCP handshake (one
round-trip, cheap CPU) and a TLS handshake (expensive: ECDHE key exchange + certificate
verification). Doing the full TLS handshake dozens of times a second pegs a single
core, at which point CPU, not the network, is the ceiling — we measured that cliff.

**TLS session resumption** (`tls.NewLRUClientSessionCache(4096)`, client.go:61). The
first handshake does the full exchange and the relay returns a session ticket; the
client caches it and later connections present it for an abbreviated handshake (roughly
symmetric-crypto only, ~10× cheaper). All connections target the same relay, so after
the first they're cheap. This is the main reason one CPU suffices.

**Pre-warmed pool** (`xport`, client.go:35). Background `warm()` goroutines
(client.go:91) dial and handshake connections ahead of time and park them in a channel
(`-pool`, default 96). `getConn` (client.go:109) takes a ready one with no handshake on
the request path. The pool also absorbs the throttle's random connection drops: a
failed dial is retried in the background (client.go:98), so the data path only sees
healthy connections.

**One request per connection** (`c.Close()`, client.go:130; HTTP/1.1 `Connection:
close`). A second request would push the connection past the quota into the drop zone,
so we never reuse one. Every request gets a fresh connection with a full quota. This
inverts normal HTTP tuning; TLS resume is what makes throwing connections away
affordable.

**Global semaphore** (`sem`, client.go:65, `-maxconns`) caps concurrent connections
across all streams so many tabs can't exhaust file descriptors or trip escalation.

---

## 13. Reliability over lossy connections

TCP gives reliability per connection. We shredded the stream across many connections,
some of which fail, so reliability is rebuilt at the app layer on three rules:

1. **Number everything.** Up- and down-chunks carry index `i`; order comes from the
   index (`upNext`/`upBuf`; `nextWrite`), never from arrival order.
2. **Dedupe on receive.** A re-sent chunk already processed is ignored (relay.go:117),
   so aggressive retry is safe.
3. **Retry on a fresh connection.** A dropped or timed-out request is treated as the
   throttle killing that connection, not a real error: retry the same indexed request
   on a new connection (download 15×, client.go:209; upload 8×; open 6×). A produced
   chunk returns in ~0.25s, so any attempt past the timeout is a dropped connection by
   definition — short timeout plus immediate retry is correct, not premature.

Timeouts match this (client.go:29-33): control ops and prefetches fail fast; the one
long-poll gets a longer budget because the relay is legitimately holding it. Under heavy
loss you get more retries and lower throughput, never corruption and never a stalled
writer. Above the SOCKS socket the app sees a normal in-order stream.

---

## 14. Throughput ceiling

`throughput ≈ (≈16 KB per connection) × (connections/sec)`. The first term is fixed.
The connection rate is bounded by CPU (mitigated by resume + pool) and by escalation:
open connections too fast, sustained, and TSPU penalizes the whole box, dropping a much
larger fraction of new connections, which lowers the effective rate.

Measured: a rested box did 7.1 Mbit/s on the default config; a back-to-back saturated
benchmark drove the same config to 0.6 Mbit/s because the benchmark's own churn tripped
escalation. The measurement perturbs the system.

Operating point: moderate parallelism, and let natural gaps in usage keep the box out
of the penalty state. Bursty browsing stays fast; a continuous max-rate download is the
workload that self-throttles. The ceiling is set by the route, not the code — which is
why more workers stops helping and starts hurting.

---

## 15. Code map

| Concern | Where |
|---|---|
| Mode dispatch, `CHUNK` size | `main.go` |
| Relay: live target conn, chunk buffer, in-order reassembly, half-close | `relay.go` |
| Greedy pre-reader slicing the download into indexed chunks | `session.reader()` |
| Blocking long-poll / instant peek / ack-based buffer drop | `readDown` / `peekDown` |
| Idempotent ordered upload write + half-close | `writeUp` / `endUp` / `maybeCloseWriteLocked` |
| HTTP endpoints `/t/o /t/u /t/e /t/d /t/c /t/stat /t/ping` | `relay.ServeHTTP` |
| SOCKS5 server | `handleSocks` |
| Pool, TLS-resume, one-request-per-conn, global semaphore | `xport` / `newXport` / `warm` / `do` |
| Raw HTTP/1.1 write + parse | `roundtrip` |
| Session open with retries | `open` |
| Upload pump | `upPump` |
| Download pump: in-order writer + parallel prefetch, X-Prod bound | `downPump` / `fetch` |
| Flags | `runClient` / `runRelay` |

Run the client with `-debug` to log a `STALL` line if the in-order writer is genuinely
stuck. `/t/stat?s=<sid>` dumps a session's live counters.
