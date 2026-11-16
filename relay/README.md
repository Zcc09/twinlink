# twinlink-relay

The relay half of Twinlink: a Speedify-style channel-bonding tunnel. It terminates a
Twinlink v1 UDP tunnel from the Android client, restores packet order, exits the traffic to
the internet through a TUN device, and spreads the replies across every link the client has,
in proportion to what each link can actually carry.

The wire format is normative and lives in [`../docs/PROTOCOL.md`](../docs/PROTOCOL.md); this
document only covers the relay.

```
client (Android)                                  relay (this program)
  TUN 10.99.0.2/24                                   TUN 10.99.0.1/24
    │  ┌──────────┐   link A (Wi-Fi)   ┌───────────────────────────┐
    ├─►│scheduler │──────────────────► │ session table, per-link   │──► reorder ──► TUN
    │  └──────────┘   link B (cell)    │ pumps, scheduler          │        │
    └───────────────────────────────►  └───────────────────────────┘        └──► internet
                                          MASQUERADE 10.99.0.0/24 → eth0
```

## Build

Go 1.27, no cgo, one dependency (`golang.org/x/sys`, used for `TUNSETIFF`/`SIOCSIFMTU`).

```sh
go build ./...                                    # host build (TUN stub off Linux)
go test ./...                                     # every test runs on any OS
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w" -o dist/twinlink-relay-linux-amd64 ./cmd/twinlink-relay
```

## Run

```sh
sudo TWINLINK_SECRET='at-least-16-bytes-shared-with-the-client' \
  ./dist/twinlink-relay-linux-amd64 \
    --listen :51821 --http 127.0.0.1:51822 \
    --tun twinlink0 --wan-iface eth0 --setup-nat
```

| flag | default | meaning |
|---|---|---|
| `--listen` | `:51821` | UDP tunnel address |
| `--http` | `127.0.0.1:51822` | control API; loopback by default because it has no auth |
| `--tun` | `twinlink0` | TUN interface name (created, MTU 1380) |
| `--mtu` | `1380` | TUN MTU, advertised in `HELLO_ACK` |
| `--client-cidr` | `10.99.0.0/24` | address pool; clients are handed `.2` upward |
| `--server-ip` | `10.99.0.1` | relay's tunnel address |
| `--secret` / `TWINLINK_SECRET` | — | shared HMAC secret, **>= 16 bytes**, never logged |
| `--session-ttl` | `90s` | idle timeout per session |
| `--queue-depth` | `4` | per-link transmit queue, in packets (latency/throughput trade-off) |
| `--setup-nat` | off | apply the idempotent iptables/sysctl/iproute2 plan, then serve |
| `--print-nat` | off | print that plan and exit (works on Windows, runs nothing) |
| `--no-stats` | off | stop sending `STATS` telemetry |
| `--no-http` | off | no control API |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` (slog text to stderr) |

### Control API

* `GET /healthz` → `200 ok`
* `GET /stats` → `{"sessions":1,"links":2,"counters":{...},"session_detail":[...]}`

## NAT

`--print-nat` renders exactly what `--setup-nat` executes. Every rule is checked with
`iptables -C` first, so running it twice is a no-op, and each added rule is re-checked
afterwards to prove it landed:

```
$ twinlink-relay-linux-amd64 --print-nat --wan-iface eth0
# twinlink-relay --setup-nat plan (idempotent: every rule is checked before it is added)
sysctl -w net.ipv4.ip_forward=1
ip link set dev twinlink0 mtu 1380
ip addr replace 10.99.0.1/24 dev twinlink0
ip link set dev twinlink0 up
iptables -t nat -C POSTROUTING -s 10.99.0.0/24 -o eth0 -j MASQUERADE || iptables -t nat -A POSTROUTING -s 10.99.0.0/24 -o eth0 -j MASQUERADE
iptables -C FORWARD -i twinlink0 -o eth0 -j ACCEPT || iptables -A FORWARD -i twinlink0 -o eth0 -j ACCEPT
iptables -C FORWARD -i eth0 -o twinlink0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT || iptables -A FORWARD -i eth0 -o twinlink0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
```

`BuildPlan` is pure Go, so the plan (and `ApplyPlan`'s idempotency, with an injected command
runner) is unit tested on Windows; only the executor is `//go:build linux`.

## Design notes

**Scheduling (PROTOCOL §5).** Deficit round robin. Every link accrues `credit += weight*dt`
and the packet goes to the largest credit, or on a tie to the lower RTT and then the lower
link id. `weight = max(rate_bps, 64 kbit/s)` while the link has been heard from inside 3 s,
else 0. `rate_bps` is an EWMA (alpha 0.3, 500 ms windows) of *bytes actually written* in this
direction; because the per-link writer is blocking, a saturated link's window count converges
to its capacity instead of to the offered load. A link whose queue is full is skipped for that
packet (its credit keeps accruing), which keeps the relay work-conserving without dropping.

**Reorder (PROTOCOL §6).** Min-heap keyed by a wrap-safe sequence comparison, `HOLD_MS = 40`,
`MAX_GAP = 64`, duplicates dropped, skip forward on timeout or gap. The relay also skips
forward when the pending set exceeds 1024 entries (a hard memory bound).

**Liveness and RTT.** One `PING` per link per second, answered with a `PONG` that echoes the
sender's 8-byte timestamp. The timestamp is microseconds since the relay started, i.e. a
monotonic epoch as §3 asks: the wall clock on Windows only ticks every 0.5-15 ms, which
quantises RTT to zero.

**One session, many links.** Links are keyed by source address, so several sockets of the same
client collapse into one session (max 8 links). A datagram from an unknown address must be a
valid `HELLO` or it is dropped: that is the only authentication in v1 (payloads are plain IP
packets, see §9 of the protocol).

## Testing

```
internal/proto    header codec, wrap-safe seq maths, HMAC vectors, control JSON
internal/sched    deficit round robin, weights, liveness, tie-breaks, rate EWMA
internal/reorder  hold, skip, duplicates, wrap at 2^32, bounded buffer
internal/relay    end-to-end over real loopback UDP with shaped links, NAT, control API
```

`internal/relay/harness_test.go` is the interesting part: an in-memory TUN device, plus a
per-link emulator that enforces capacity with a token bucket *in the write path* (so the
relay's "bytes written" estimator measures capacity, not offered load) and applies latency as
a timestamped release (so it does not serialise the link at 1/latency). The client side of the
tunnel is a second reorder buffer that verifies index and contents of every packet.

Measured on this machine (`go test ./internal/relay -bench . -benchtime 10s -run XXX`):

| configuration | goodput |
|---|---|
| link 0 alone (50 Mbit/s, 10 ms) | 6.06-6.20 MB/s (97-99% of its cap) |
| link 1 alone (20 Mbit/s, 50 ms) | 2.23-2.48 MB/s (89-99% of its cap) |
| both bonded | **8.67-8.68 MB/s (69.4 Mbit/s, 99% of the 70 Mbit/s sum)** |
| speed-up vs the best single link | **1.40-1.43x** |

And the end-to-end split (`go test ./internal/relay -run TestRelayEndToEndBonding -v`):

```
split: link0 2208000 B (80.0%), link1 552000 B (20.0%), ratio 4.00 (want 4.00)
client reorder stats: delivered=8000 dup=0 skipped=0, max inter-arrival 2.653ms
```

### Where the implementation deviates, and why

1. **HELLO proof: hex(session) is four zero-padded digits.** `ProofMessage` renders the
   session as `%04x` (`"twinlink-v1|1a2b|1737000000"`), matching both vectors in the protocol.
   `VerifyProof` also accepts the unpadded form so a client that formats it as `%x` still
   interoperates; that cannot weaken authentication, only widen what is accepted.

2. **Reorder: `expected` starts at 0.** §2 says DATA sequence numbers start at 0 per session
   and direction, so the receiver knows its baseline instead of inventing it from the first
   packet that happens to arrive. Without this, a reordered *first* packet is
   indistinguishable from a stale duplicate and is dropped. The unit tests that pin §6
   semantics use the implicit baseline, so both behaviours are covered.

3. **Reorder: receiver-stall guard (`reorder.WithFreezeGuard`, on in the relay, off in the
   §6 conformance tests).** If no packet at all has arrived for a hold period, the buffer
   assumes *it* was not running (process suspended, Android Doze, host stall), refreshes the
   pending ages and grants a recovery grace of `stall + HOLD_MS` before age-based skips resume.
   Rationale: on resume every pending packet looks ancient for reasons that have nothing to do
   with loss, and the packets still queued in the other link's socket buffer arrive a few
   milliseconds later. Without the guard, every resume costs a burst of skipped packets. The
   hold/gap thresholds themselves are untouched, and gap-based skips are not deferred.

4. **`--queue-depth` defaults to 4 packets.** Depth is a genuine trade-off: each queued packet
   is latency on the receiving side (2.2 ms per packet on a 5 Mbit/s link, which eats the 40 ms
   reorder budget), but a depth of 2 costs about a third of a fast link's throughput because
   every packet then needs its own goroutine hand-off. 4 keeps an emulated 50 Mbit/s link at
   97% of its cap while leaving the reorder budget intact.

5. **`--setup-nat` also brings the TUN interface up** (`ip link set`, `ip addr replace`,
   `ip link set up`). The protocol only asks for the iptables/sysctl rules, but a relay whose
   interface has no address never passes a packet. The commands are printed by `--print-nat`
   like the rest of the plan.

6. **The end-to-end test uses 20 ms and 30 ms RTT** rather than 20/80. With RTT applied per
   direction (RTT/2 each way) a 20/80 pair leaves a 30 ms one-way spread inside a 40 ms hold
   budget, i.e. 10 ms of head room for queueing and host jitter. This host occasionally
   deschedules the whole test process for 30-140 ms (measured: clean inter-arrival gaps, no GC
   pause, no blocked UDP write, in a process whose sleeps are otherwise accurate to 1.4 ms), so
   an exactly-once assertion at 20/80 fails a few percent of runs for reasons that have nothing
   to do with the relay. 5 ms of spread keeps the links genuinely different while leaving the
   hold budget intact; the capacity ratio (4:1) and the split assertion are unchanged. The
   test retries a run only when the failures coincide with a detected >20 ms arrival gap, and
   says so in the test log; steady state inter-arrival is ~3 ms.

## Container

```sh
export TWINLINK_SECRET='at-least-16-bytes-shared-with-the-client'
docker compose -f deploy/compose.yaml up --build
```

`deploy/compose.yaml` adds `NET_ADMIN`, `/dev/net/tun`, `net.ipv4.ip_forward=1`, publishes
`51821/udp` and `127.0.0.1:51822/tcp`, and passes the secret through `TWINLINK_SECRET`.
