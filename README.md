# cyclevpn

A small tunnel that defeats Russia's (TSPU) **per‑connection byte‑quota throttle**
by spreading every TCP stream across many short‑lived HTTPS requests, each kept
under the quota, so full‑speed traffic is reassembled from many cheap connections.

It is a two‑hop design so you can use a **standard phone app** (Shadowrocket /
v2rayNG / NekoBox) with no custom client on the phone.

> This README is the **deployment** guide. For the mechanism — how Russia detects and
> throttles, why standard tools fail against it, and how the slicing works down to the
> packet and kernel level — see **[HOW-IT-WORKS.md](HOW-IT-WORKS.md)**.

---

## The problem this solves

From a Russian network, connecting straight to a foreign server is throttled to
near zero for anything but tiny requests. We measured the mechanism with `tcpdump`:

* TCP + TLS handshake completes normally.
* The server delivers the **first ~16–26 KB at full speed**.
* Then **every further packet is silently dropped, in both directions** — no RST,
  no TCP zero‑window. The connection just starves and dies.

So it is a **per‑connection byte quota (~16 KB), keyed on the destination’s domain
(SNI)**. Proof: to the *same* Cloudflare IP, `speed.cloudflare.com` (a whitelisted
name) ran at 30 MB/s while our own domain on the identical IP got ~1 KB/s.

Every other evasion we tried was defeated: REALITY, Hysteria2, XHTTP/SplitHTTP
(its download is one long stream → hits the quota), TLS fragmentation / byedpi
desync, domain fronting (Cloudflare 403s it), ECH (Russia blocks it).

**What works:** the quota is *per connection*. Open many short connections, take
each one’s ~16 KB, and cycle. That is what `cyclevpn` does — for *both* directions.

---

## Architecture (two hops)

```
   your phone                RuVDS box (INSIDE Russia)            Contabo box (outside RU)
  ┌──────────┐   standard   ┌────────────────────────┐   cycling  ┌──────────────────────┐
  │Shadowrock│──VLESS/TCP──▶│ xray VLESS entry :2053  │  transport │ cyclevpn relay        │──▶ internet
  │  et app  │  (domestic,  │  → cyclevpn client      │═══════════▶│ (behind Caddy TLS)    │
  └──────────┘  not throttled)  (SOCKS 127.0.0.1:10900)│  many short │                       │
                              └────────────────────────┘  HTTPS reqs └──────────────────────┘
```

* **Phone → entry** is *domestic* (Russia→Russia); TSPU does not throttle it, so a
  plain standard protocol works at full speed.
* **entry → exit** is the throttled foreign leg — carried by the cycling transport.
* **exit** reaches the open internet; its IP is what sites see.

---

## Build

```bash
go build -o cyclevpn .
# cross builds:
GOOS=linux GOARCH=amd64 go build -o cyclevpn-linux-amd64 .
GOOS=linux GOARCH=arm64 go build -o cyclevpn-linux-arm64 .
```

`cyclevpn` has two modes:

```
cyclevpn relay  -listen 127.0.0.1:8791
cyclevpn client -url https://EXIT_DOMAIN -listen 127.0.0.1:10900 -workers 24
```

---

## Deploy — EXIT node (outside Russia, e.g. Contabo Germany)

1. Point a DNS **A record** at the box (DNS‑only if it’s on Cloudflare), e.g.
   `exit.example.com → <box IP>`.
2. Copy this repo (with a built `cyclevpn-linux-amd64`) to the box and run:

   ```bash
   sudo ./install-exit.sh exit.example.com
   ```

   This installs Caddy (auto HTTPS), installs the `cyclevpn relay` service, opens
   the firewall, and verifies `https://exit.example.com/t/ping` returns `pong`.

**Redeploying on a brand‑new exit box is just those two steps.**

---

## Deploy — ENTRY node (inside Russia, e.g. RuVDS)

```bash
sudo ./install-entry.sh exit.example.com 2053
```

This installs `xray` (the standard VLESS server your phone talks to), the
`cyclevpn client` service (cycles out to the exit), wires them together, opens the
port, and prints a `vless://…` link **and a scannable QR** for your phone.

---

## Phone

Install **Shadowrocket** (iOS) or **v2rayNG / NekoBox** (Android), scan the QR from
the entry install output, connect. Done.

If you rebuild the exit on a new box, just re‑run `install-entry.sh` pointing at the
new domain (or edit `/etc/systemd/system/cyccli.service`) — the phone QR does not
change (it only references the entry box).

---

## How can this carry a big download, or a video stream, or a call?

This is the part that seems impossible at first: if the firewall kills every
connection after ~16 KB, how do you download a 100 MB file or watch YouTube?

The trick is that **the stream your app sees and the connections on the wire are
two different things.**

* On the **exit** box, `cyclevpn` opens **one normal, long-lived TCP connection**
  to the real destination (say `youtube.com`) and keeps it open. From YouTube's
  point of view there is a single, continuous connection — it never sees any
  cycling. The exit reads that stream and chops it into numbered ~14 KB chunks:
  `0, 1, 2, 3, …`, buffering them.
* On the **entry** box (in Russia), the client pulls those chunks over **many
  separate short HTTPS connections** — connection A grabs chunk 0, connection B
  grabs chunk 1, C grabs chunk 2, all at the same time. Each connection carries
  only its one chunk (well under the 16 KB quota), so the firewall lets it
  through at full speed, then the connection is thrown away. There are always
  fresh ones being opened.
* The client reassembles the chunks back into order (`0,1,2,3,…`) and hands your
  app a **single, continuous TCP stream** — exactly what it sent on the exit side.

So a 100 MB download is really ~7,000 chunks pulled over ~7,000 short-lived
connections (opened and discarded continuously, dozens in flight at once) and
glued back into one file. Your browser just sees a normal download.

**Throughput** = (chunks in flight in parallel) × 16 KB ÷ (time per connection).
With ~16–24 parallel connections that is several Mbit/s from a 1‑CPU box — enough
for browsing and **SD/HD video**. More parallel connections (a bigger entry box)
= more speed. Video also **buffers**, which smooths over the small variations as
connections cycle, so playback stays steady.

**Long-lived / persistent connections** (a chat socket, a streaming video, a
download that takes minutes) stay alive because **the real connection lives on the
exit box and is never cycled** — only the invisible entry↔exit transport is made
of short connections. The app’s connection can stay open for hours.

**Voice calls (WhatsApp/Telegram) work** via a dedicated UDP path (SOCKS5 `UDP
ASSOCIATE`, tuned for latency not throughput). Measured through the tunnel at
voice bitrate: ~80 ms median RTT, p99 ~115 ms, ~0.1% loss on a rested box — call
quality, and it survives past the throttle where a single UDP flow would die in
~8 s. No config needed; a phone that routes UDP through the tunnel just works.
See **[HOW-IT-WORKS.md](HOW-IT-WORKS.md)** §10.

**Video calls are marginal** (higher bitrate → more connection churn → ~7%+ loss);
voice is the reliable target. If a call app misbehaves you can still **split-tunnel**
it (route it DIRECT), but note direct calls are often throttled in Russia too, which
is why carrying them through the tunnel is worthwhile.

## How it works internally

* **relay** (exit): holds the live TCP connection to each destination. Buffers the
  download as indexed chunks and reassembles the upload in order, so the client can
  drive both directions over many independent short requests. Endpoints:
  `POST /t/o` (open host:port → session id), `POST /t/u?s=&i=` (upstream chunk N),
  `GET /t/d?s=&i=&a=` (downstream chunk N, `a`=ack lets the relay drop consumed
  chunks), `POST /t/c` (close).
* **client** (entry): a SOCKS5 server. For every stream it opens a session, pumps
  uploads as sequential `POST`s, and fetches downloads with a sliding window of
  parallel `GET`s written back in order. Each HTTP request is a **fresh TCP
  connection** (`Connection: close`) so it stays under the quota, but the TLS
  handshake is **resumed** from a session cache (`tls.ClientSessionCache`) so the
  many connections stay cheap on a 1‑CPU box.

Tune `-workers` (parallel download connections per stream). More workers = more
throughput until the box’s CPU (TLS handshakes) or a connection‑rate limit caps it.

---

## Status / limits

* Beats the throttle; real pages load at a few Mbit/s from a tiny 1‑CPU entry box.
  A beefier entry box or higher `-workers` goes faster.
* Sustained single big‑file downloads are more variable than page loads.
* The phone→entry hop relies on domestic Russian traffic being un‑throttled (true
  today). If Russia ever throttles domestic proxy traffic, add TLS/obfuscation to
  the entry inbound.

Not affiliated with any provider. Use responsibly and legally.

## Tuning (all flags)

Every knob is a flag so you can sweep for your box/route:

**client:** `-workers` (download prefetch window per stream), `-pool` (pre-warmed
connections), `-maxconns` (global connection cap), `-chunk` (bytes/request, keep
< ~16 KB), `-fetch-timeout` / `-poll-timeout` / `-ctrl-timeout`.
**client (UDP/voice):** `-ubatch` (outgoing datagram coalescing window),
`-upollers` (return long-polls kept in flight per call).
**relay:** `-chunk`, `-hold` (TCP long-poll wait), `-uhold` (UDP return long-poll wait).

**Important — more is NOT always better, and you cannot benchmark your way
higher.** Throughput is bounded by *(bytes allowed per connection ≈ 16 KB) ×
(rate you can open connections)*. The chunk size is already near the quota, so the
only lever is connection rate — but opening connections *too* aggressively makes
the firewall **escalate** its throttling and clamp the whole box.

Measured on the 1-CPU RuVDS entry box: on a **rested** box the moderate default
`-workers 32 -pool 96 -maxconns 128 -fetch-timeout 1500ms` did **7.1 Mbit/s at
20/20 reliability**. Raising `-workers` to 48 or 64 did **not** help (≈2.5 Mbit/s);
a shorter `-fetch-timeout` (900 ms) **hurt** (1.6 Mbit/s). And crucially, a
back-to-back saturated benchmark sweep **collapsed the same config from 7.1 to
0.6 Mbit/s** — because the sweep itself is the connection churn that triggers the
escalation. It recovers after the box sits idle for a while.

Takeaways: the moderate default is the sweet spot; the ceiling is route-imposed,
not config-imposed; and **sustained full-saturation traffic self-throttles** —
normal bursty browsing (with idle gaps) stays fast, continuous max-rate downloads
degrade. The client uses only ~15–30 MB RAM even with a large pool.

If a `cyclevpn client` service ever returns dead connections (`000`) while a
freshly launched client works, the long-lived instance has wedged its pool —
`systemctl restart cyccli` clears it.
