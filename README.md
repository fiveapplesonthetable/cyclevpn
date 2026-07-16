# cyclevpn

A small tunnel that defeats Russia's (TSPU) **per‑connection byte‑quota throttle**
by spreading every TCP stream across many short‑lived HTTPS requests, each kept
under the quota, so full‑speed traffic is reassembled from many cheap connections.

It is a two‑hop design so you can use a **standard phone app** (Shadowrocket /
v2rayNG / NekoBox) with no custom client on the phone.

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
