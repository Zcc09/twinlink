# Twinlink

Channel bonding for Android: your phone's Wi-Fi and cellular links carry one tunnel at the same
time, so a single download runs at roughly the sum of both instead of whichever link Android picked.

It is the same idea as Speedify: a client-side VPN captures every IP packet, spreads the packets
across all available links, and a small relay you own re-orders them, exits them to the internet and
splits the replies back over the same links.

```
   phone (Twinlink app)                                  relay (Linux, public IP)
 ┌────────────────────────┐                        ┌────────────────────────────────┐
 │  TUN 10.99.0.2  mtu1380│    UDP :51821          │  TUN 10.99.0.1  mtu1380        │
 │  reorder ⇄ scheduler   │◄──────────────────────►│  reorder ⇄ scheduler           │
 │      ╱          ╲      │  link A: Wi-Fi         │        │                       │
 │  socket A     socket B │  link B: cellular      │        └──► NAT ──► internet    │
 └────────────────────────┘                        └────────────────────────────────┘
```

## Repo layout

| path | what |
|---|---|
| `docs/PROTOCOL.md` | the wire format and algorithms — the contract both halves implement |
| `app/` | Android client (Kotlin, `VpnService`, plain Views) |
| `relay/` | relay server (Go, single static binary, Linux) |
| `tools/e2e/` | independent protocol client used to test a relay without a TUN device |
| `tools/build_app.bat` | local Android build entry point (JDK/SDK/Gradle under `~/tools`) |

## How the bonding actually works

1. **Capture.** `VpnService` gives the app a TUN interface and, because the app adds
   `0.0.0.0/0`, every IPv4 packet the phone sends or receives passes through it. The app's own
   sockets are exempted (`addDisallowedApplication`), so the tunnel never loops into itself.
2. **Links.** One UDP socket per usable network, each bound to its own `android.net.Network`
   (`Network.bindSocket`). Android hands us Wi-Fi and cellular at the same time; we use both.
3. **Split.** A deficit-round-robin scheduler weights each link by its *measured achieved
   throughput* (EWMA over 500 ms windows). A link that carries more gets a bigger share, so the
   split converges on the real capacity ratio — 4:1 for 80 Mbps Wi-Fi against 20 Mbps LTE — and a
   dead link drops out after 3 s of silence.
4. **Reorder.** Packets arrive out of order by design. Each side holds a small reorder buffer and
   releases packets in sequence order, giving up on a hole after 40 ms or 64 missing sequence
   numbers so a real loss does not stall TCP.
5. **Exit.** The relay writes the packets into its own TUN and NATs them out of its WAN interface,
   then schedules the replies back across the same links using the same algorithm in reverse.

Loss is not retransmitted — TCP above the tunnel recovers, exactly as it would over a lossy radio.

## Relay deployment

The relay must be reachable from **both** links, so it lives on a public IP: a small VPS, or the
same host you already run other services on if it has real inbound UDP.

```bash
# build a static Linux binary (from Windows, macOS or Linux)
cd relay && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
  -o dist/twinlink-relay-linux-amd64 ./cmd/twinlink-relay

# on the server
sudo ./twinlink-relay-linux-amd64 \
  --listen :51821 \
  --secret "$(cat /etc/twinlink.secret)" \
  --setup-nat --wan-iface eth0 \
  --http 127.0.0.1:51822
```

Docker: `relay/deploy/compose.yaml` (needs `NET_ADMIN`, `/dev/net/tun`, UDP 51821 published).

Client side: enter the relay host, port and the same secret in the app, then flip the switch and
accept the Android VPN prompt.

## Verifying a relay

`tools/e2e` speaks the protocol without a TUN device, so a relay can be tested from any machine:

```bash
cd tools/e2e && go build -o e2e .
./e2e --host <relay-ip> --port 51821 --secret <secret> --links 2 --icmp 12 --dns 60
```

It registers two links, sends ICMP echo and DNS queries inside hand-built IPv4 packets through the
tunnel, and asserts that the replies come back on both links. `PASS` means the whole path —
client → relay → TUN → NAT → internet → back — is working.

## What has actually been measured

| check | result |
|---|---|
| relay tests (`go test ./...`, 91 assertions incl. subtests) | pass |
| relay bonding rig — 8000 packets over two emulated 4:1 links | every packet delivered exactly once, in order; split 80.0 / 20.0 |
| relay benchmark — 50 Mbit/s + 20 Mbit/s emulated links | **8.67 MB/s bonded vs 6.12 MB/s best single link = 1.42x** |
| relay on real Linux (container on the NAS, kernel TUN + MASQUERADE) | ICMP echo 12/12, DNS 40/40 answered through the tunnel, both links carried bytes |
| app JVM tests | 34 pass (codec, HMAC vectors, reorder, scheduler, engine) |
| app on hardware — Galaxy Tab S9 Ultra, Android 16 | VPN consent → `Connected`, session 1297, Wi-Fi link at RTT 20 ms, 2 MB / 1699 packets through the relay in 8 s, clean disconnect |

Not yet measured: end-to-end bonding on a phone with Wi-Fi **and** cellular at once. That needs a
relay both links can reach, which a home line behind CGNAT cannot provide.

## Status / limits (v1)

Working: dual-link capture, weighted splitting, reordering, NAT exit, session auth, live per-link
stats in the UI.

Not in v1, stated plainly so nobody is surprised:

- **No encryption.** Payloads are plain IP packets inside UDP; only the session handshake is
  HMAC-authenticated. That is fine for bonding a link you control, and it is *not* a privacy VPN.
  Per-packet ChaCha20-Poly1305 is the next protocol version.
- **No FEC/redundancy mode** (Speedify's fallback for very lossy mobile links).
- **No IPv6** tunnelling; the app deliberately does not route IPv6 into the tunnel.
- Bonding is only as fast as the relay's own uplink and its distance from you — a relay in the same
  country adds ~5-15 ms RTT, one on another continent adds 100 ms+ to every connection.
- Links are bonded per packet, so the two links must be *similarly* fast on latency: a hole is only
  held 40 ms, so a link more than ~40 ms slower than the best one gets its packets skipped (they are
  treated as loss). Wi-Fi + LTE in the same place is fine; Wi-Fi + a congested satellite link is not.
