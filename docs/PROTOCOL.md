# Twinlink Protocol v1

Twinlink bonds several network paths (phone Wi-Fi + phone cellular, or any mix) into one
IP tunnel, the way Speedify does. The phone captures all traffic in a local `VpnService` TUN,
sends every IP packet to a relay you control over **all** available links, and the relay
re-orders, exits the traffic to the internet, and splits the replies back across the links.

```
        Android (Twinlink app)                       Linux relay (twinlink-relay)
   ┌──────────────────────────┐                ┌──────────────────────────────────┐
   │ TUN 10.99.0.2/24  mtu1380│                │ TUN 10.99.0.1/24  mtu1380        │
   │   ▲            │         │                │   ▲              │               │
   │   │ IP pkts    │         │   UDP :51821   │   │              │  MASQUERADE   │
   │ reorder     scheduler    │◄──────────────►│ reorder      scheduler           │
   │   △            │         │   link A Wi-Fi │   △              │               │
   │   │            ├─────────┼───────────────►│   │              ▼               │
   │   │            │         │   link B cell  │   │        internet (eth0)       │
   └───┴────────────┴─────────┘                └───┴──────────────────────────────┘
```

Everything in this document is normative. Both implementations must agree byte for byte.

## 1. Transport

- UDP only. Default relay port **51821**.
- One UDP socket per local link. Every socket is bound to a specific interface
  (Android: `Network.bindSocket`; relay: one listening socket, peers distinguished by source address).
- All integers big-endian (network byte order).

## 2. Packet header — 12 bytes, present on every datagram

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|0 0 0 1| type  |     flags     |          session id           |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                          sequence                             |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|         payload length        |   link id     |   reserved    |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                        payload ...                            |
```

| offset | size | field | meaning |
|---|---|---|---|
| 0 | 1 | `ver_type` | high nibble `version` = **1**, low nibble `type` |
| 1 | 1 | `flags` | bit0 `LAST_FRAG` (unused in v1, always 0), bit1 `AUTH` (HELLO only), rest 0 |
| 2 | 2 | `session` | session id, random `u16` chosen by the client, never 0 |
| 4 | 4 | `seq` | per-direction, per-session sequence number starting at 0, `u32` wraps |
| 8 | 2 | `length` | payload bytes, `0 <= length <= 1400` |
| 10 | 1 | `link_id` | sender's link index, informational (receiver MAY use for stats) |
| 11 | 1 | `reserved` | 0 |

`DATA` sequence numbers are independent of `PING`/`HELLO` sequence numbers: telemetry
control packets carry `seq = 0` and are never fed to the reorder buffer.

## 3. Message types

| type | name | direction | payload |
|---|---|---|---|
| 1 | `HELLO` | client → relay | JSON (see §4) |
| 2 | `HELLO_ACK` | relay → client | JSON (see §4) |
| 3 | `DATA` | both | exactly one IPv4 packet |
| 4 | `PING` | client → relay, relay → client | 8-byte `u64` microseconds, sender monotonic-ish epoch |
| 5 | `PONG` | answer to `PING` | the same 8 bytes echoed back |
| 6 | `STATS` | relay → client | JSON, optional, may be ignored |
| 7 | `BYE` | both | JSON `{"err":"..."}`, session is dead |

`DATA` payloads whose first nibble is not `4` (IPv4) are dropped and counted as `bad_payload`.

## 4. Handshake and session

Shared secret `S` (a UTF-8 string, configured on both sides, minimum 16 bytes).

Client, on **each** link, immediately after the socket is bound:

```
session = random u16 (non-zero)
ts      = unix seconds
proof   = lowercase hex( HMAC-SHA256( key = S, msg = "twinlink-v1|" + hex(session) + "|" + decimal(ts) ) )
HELLO   payload = {"v":1,"ts":<ts>,"proof":"<proof>","mtu":1380,"name":"<optional label>"}
```

Relay rules:
- `abs(now - ts) > 120` → send `BYE {"err":"clock"}` and ignore the source address.
- proof mismatch → send `BYE {"err":"auth"}` and ignore the source address.
- otherwise create or refresh the session, register the datagram's **source address** as one of
  that session's links (`link_id` assigned sequentially per new source address, 0..7), and reply
  `HELLO_ACK {"v":1,"client_ip":"10.99.0.2","server_ip":"10.99.0.1","mtu":1380,"max_payload":1400}`.

`HELLO_ACK` is sent to the source address the `HELLO` arrived from.

Any datagram from a source address that is not yet a registered link of an authenticated session
must be a valid `HELLO`; otherwise it is dropped. This is what stops a third party from injecting
a link into somebody's session, and it stops the relay being used without the secret.

Clients re-send `HELLO` on every link every 30 s (keeps NAT mappings and DHCP-ish rebindings alive).

**Addresses are per session, and clients must use theirs.** The relay allocates one address out of
the pool per session and returns it in `HELLO_ACK.client_ip`; a DATA packet whose IPv4 source is not
the session's own address is dropped and counted as `foreign_source`. That check is what keeps two
devices apart: if both hardcode `10.99.0.2`, the second one's packets would be NATed as the first
one's address and each would receive a share of the other's replies. Clients therefore take the TUN
address from `HELLO_ACK` (a short pre-flight handshake before the interface is created), never from
a constant.

Session expiry on the relay: 90 s with no datagram from any link → drop the session and its TUN routes.
A session holds at most 8 links. v1 relay serves any number of sessions from a pool
`10.99.0.0/24` (`.1` = relay, clients from `.2` upward).

## 5. Scheduling outbound packets (both directions)

Both sides run the same scheduler. One instance per direction.

State per link:

- `alive` — `true` while a `PONG` (or any datagram) has been seen in the last 3 s.
- `rtt_us` — EWMA (alpha 0.2) of measured round trip from `PING`/`PONG`.
- `rate_bps` — EWMA (alpha 0.3, sampled every 500 ms) of **achieved** tunnel throughput on that link
  in this direction: `bytes_this_window * 8 / window_s`. Starts at 0.
- `weight` — `max(rate_bps, 64_000)` when alive, else 0.

Selection is **deficit round robin over the weights**: each link accumulates
`credit += weight * dt` every tick; a packet is handed to the link with the largest credit,
which then has `credit -= packet_bytes * 8`. Ties break toward the lower `rtt_us`, then lower `link_id`.
This splits traffic proportionally to measured capacity and converges to the faster link on its own
(a link that carries more gets a higher weight), while a dead link stops receiving after 3 s.

For a single stream the weight convergence is the whole point: both links carry DATA packets of the
same flow, which is what makes the phone's single TCP download faster than either link alone.

**RTT spread limit.** A hole is only waited for `HOLD_MS` (40 ms), so a link whose packets land more
than ~40 ms after the fastest link's will have them skipped as losses — its bandwidth is spent and
thrown away, and the flow sees a spurious retransmit. Bond links whose RTT differs by less than
about 30 ms (the normal case for Wi-Fi + LTE/5G on the same site). A relay far from the phone adds
the same latency to both links, which is harmless; it is the *difference* between links that hurts.

## 6. Reordering (receiver)

`DATA` packets arrive out of order by design. The receiver must restore order before writing to the TUN:

```
expected = highest_delivered + 1
pending  = min-heap keyed by seq (wrapping u32 comparison)
on DATA(seq):
    if seq < expected (i.e. already delivered, wrap-safe): drop as duplicate
    insert into pending
    drain()

drain():
    while pending contains expected: write to TUN, expected++
    if pending not empty:
        oldest_age = now - arrival_time(min_pending)
        gap        = seq_distance(expected, min_pending_seq)
        if oldest_age > HOLD_MS or gap > MAX_GAP:
            expected = min_pending_seq      # give up on the hole and skip forward
            continue drain()
```

`HOLD_MS = 40`, `MAX_GAP = 64`. A hole longer than either bound is treated as a real loss;
skipping forward promptly is what keeps TCP from stalling.

## 7. Reliability rules

- UDP is lossy. Twinlink does **not** retransmit packets — TCP above it recovers, exactly as it
  would over a lossy radio. `HOLD_MS` bounds how long recovery is delayed.
- `payload length > 1400` or `length != datagram_len - 12` → drop, count `malformed`.
- The TUN MTU on both ends is **1380**, so a well-behaved stack never produces an oversized packet
  (1380 + 12 header + 8 UDP + 20 IP = 1420, inside a 1500-byte path and inside typical mobile 1400+).
- `seq` wrap: all comparisons are `int32(seq_a - seq_b) < 0` style, never plain `<`.
- IPv6 is not tunnelled in v1. The Android client must not route IPv6 into the TUN.

## 8. Telemetry (`STATS`, optional)

Relay sends every 2 s per link: `{"links":[{"id":0,"rx_bps":123456,"tx_bps":234567,"rtt_ms":21.4}],
"total_rx_bps":0,"total_tx_bps":0,"sessions":1}`. Receivers may ignore unknown fields.

## 9. What v1 deliberately leaves out

- **No encryption.** Payloads are plain IP packets inside UDP, authenticated only at session setup
  (HMAC). Do not ship it to users as a privacy VPN; treat it as a bonding tunnel over a link you own.
  Per-packet AEAD (ChaCha20-Poly1305, 4-byte session salt + 8-byte seq nonce) is the v2 item; the
  header already carries a `flags` bit for it.
- No FEC / redundancy mode (Speedify's "redundant" fallback for lossy mobile links).
- No IPv6, no per-session bandwidth caps, no accounting.

## 10. Ports and paths

| item | value |
|---|---|
| relay UDP port | 51821 |
| relay TUN | `10.99.0.1/24`, mtu 1380 |
| client TUN | `10.99.0.2/24`, mtu 1380, all IPv4 routed into it |
| client DNS | `1.1.1.1` (goes through the tunnel) |
| control HTTP (relay) | `:51822` — `GET /healthz`, `GET /stats` (JSON, no auth, bind loopback by default) |
