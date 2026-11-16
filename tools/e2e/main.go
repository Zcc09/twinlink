// Command e2e is an independent Twinlink protocol client used to test a relay
// from a machine that cannot create a TUN device (Windows desktop, CI).
//
// It registers one or more "links" (UDP sockets) with the relay over HELLO,
// then injects real IPv4 packets (ICMP echo and DNS queries) into the tunnel.
// Replies come back as DATA packets, which proves the whole path:
// client -> relay -> TUN -> kernel NAT -> internet -> relay -> client.
//
// This file deliberately re-implements the wire format from docs/PROTOCOL.md
// instead of importing the relay's packages: a second implementation is what
// catches a spec misread.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"time"
)

const (
	hdrLen = 12

	tHello    = 1
	tHelloAck = 2
	tData     = 3
	tPing     = 4
	tPong     = 5
	tStats    = 6
	tBye      = 7

	clientIP = "10.99.0.2"
)

func hdr(typ byte, session uint16, seq uint32, linkID byte, payload []byte) []byte {
	b := make([]byte, hdrLen+len(payload))
	b[0] = byte(0x10 | typ)
	b[1] = 0
	binary.BigEndian.PutUint16(b[2:], session)
	binary.BigEndian.PutUint32(b[4:], seq)
	binary.BigEndian.PutUint16(b[8:], uint16(len(payload)))
	b[10] = linkID
	b[11] = 0
	copy(b[hdrLen:], payload)
	return b
}

type parsed struct {
	typ     byte
	session uint16
	seq     uint32
	length  uint16
	linkID  byte
	payload []byte
}

func parse(b []byte) (parsed, error) {
	if len(b) < hdrLen {
		return parsed{}, fmt.Errorf("short datagram %d", len(b))
	}
	if b[0]>>4 != 1 {
		return parsed{}, fmt.Errorf("bad version %d", b[0]>>4)
	}
	p := parsed{
		typ:     b[0] & 0x0f,
		session: binary.BigEndian.Uint16(b[2:]),
		seq:     binary.BigEndian.Uint32(b[4:]),
		length:  binary.BigEndian.Uint16(b[8:]),
		linkID:  b[10],
	}
	if int(p.length) != len(b)-hdrLen {
		return parsed{}, fmt.Errorf("length field %d != actual %d", p.length, len(b)-hdrLen)
	}
	p.payload = b[hdrLen:]
	return p, nil
}

func proof(secret string, session uint16, ts int64) string {
	msg := fmt.Sprintf("twinlink-v1|%04x|%d", session, ts)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// ---- IPv4 / ICMP / UDP / DNS construction ----------------------------------

func ipChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func ipv4Packet(proto byte, src, dst string, l4 []byte) []byte {
	s := net.ParseIP(src).To4()
	d := net.ParseIP(dst).To4()
	pkt := make([]byte, 20+len(l4))
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:], uint16(len(pkt)))
	binary.BigEndian.PutUint16(pkt[4:], uint16(time.Now().UnixNano()&0xffff)) // id
	binary.BigEndian.PutUint16(pkt[6:], 0x4000)                              // DF
	pkt[8] = 64
	pkt[9] = proto
	copy(pkt[12:], s)
	copy(pkt[16:], d)
	binary.BigEndian.PutUint16(pkt[10:], ipChecksum(pkt[:20]))
	copy(pkt[20:], l4)
	return pkt
}

func icmpEcho(id, seq uint16, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	b[0] = 8 // echo request
	b[4] = byte(id >> 8)
	b[5] = byte(id)
	b[6] = byte(seq >> 8)
	b[7] = byte(seq)
	copy(b[8:], payload)
	binary.BigEndian.PutUint16(b[2:], ipChecksum(b))
	return b
}

func udpSeg(sport, dport uint16, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(b[0:], sport)
	binary.BigEndian.PutUint16(b[2:], dport)
	binary.BigEndian.PutUint16(b[4:], uint16(8+len(payload)))
	// UDP checksum left 0 (legal for IPv4)
	copy(b[8:], payload)
	return b
}

func dnsQuery(txid uint16, name string, qtype uint16) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[0:], txid)
	binary.BigEndian.PutUint16(b[2:], 0x0100) // RD
	binary.BigEndian.PutUint16(b[4:], 1)      // QDCOUNT
	for _, label := range splitLabels(name) {
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	b = append(b, 0)
	var q [4]byte
	binary.BigEndian.PutUint16(q[0:], qtype)
	binary.BigEndian.PutUint16(q[2:], 1)
	return append(b, q[:]...)
}

func splitLabels(name string) []string {
	var out []string
	cur := ""
	for _, c := range name {
		if c == '.' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// ---- link ------------------------------------------------------------------

type link struct {
	id   byte
	name string
	conn *net.UDPConn
	relay *net.UDPAddr
	tx   int64
	rx   int64
}

func newLink(name string, id byte, relay *net.UDPAddr) (*link, error) {
	c, err := net.DialUDP("udp", nil, relay)
	if err != nil {
		return nil, err
	}
	c.SetReadBuffer(4 << 20)
	c.SetWriteBuffer(4 << 20)
	return &link{id: id, name: name, conn: c, relay: relay}, nil
}

func (l *link) hello(session uint16, secret string) error {
	ts := time.Now().Unix()
	pl, _ := json.Marshal(map[string]any{
		"v": 1, "ts": ts, "proof": proof(secret, session, ts), "mtu": 1380, "name": l.name,
	})
	if _, err := l.conn.Write(hdr(tHello, session, 0, l.id, pl)); err != nil {
		return err
	}
	l.tx += int64(hdrLen + len(pl))
	buf := make([]byte, 2048)
	l.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := l.conn.Read(buf)
	if err != nil {
		return fmt.Errorf("no HELLO_ACK: %w", err)
	}
	p, err := parse(buf[:n])
	if err != nil {
		return fmt.Errorf("bad ack: %w", err)
	}
	switch p.typ {
	case tHelloAck:
		return nil
	case tBye:
		return fmt.Errorf("relay refused session: %s", string(p.payload))
	default:
		return fmt.Errorf("unexpected reply type %d", p.typ)
	}
}

func (l *link) send(typ byte, session uint16, seq uint32, payload []byte) error {
	b := hdr(typ, session, seq, l.id, payload)
	n, err := l.conn.Write(b)
	l.tx += int64(n)
	return err
}

// reader drains one link, handing every DATA payload to out.
func (l *link) reader(out chan<- []byte, stop <-chan struct{}) {
	buf := make([]byte, 4096)
	for {
		select {
		case <-stop:
			return
		default:
		}
		l.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := l.conn.Read(buf)
		if err != nil {
			continue
		}
		p, perr := parse(buf[:n])
		if perr != nil {
			continue
		}
		switch p.typ {
		case tData:
			l.rx += int64(n)
			cp := make([]byte, len(p.payload))
			copy(cp, p.payload)
			select {
			case out <- cp:
			default: // never block the link
			}
		case tPing:
			l.send(tPong, p.session, 0, p.payload)
		}
	}
}

// ---- main ------------------------------------------------------------------

func main() {
	host := flag.String("host", "192.168.0.172", "relay host")
	port := flag.Int("port", 51821, "relay udp port")
	secret := flag.String("secret", "", "shared secret")
	links := flag.Int("links", 2, "number of links to register")
	icmpCount := flag.Int("icmp", 12, "icmp echo requests to send")
	dnsCount := flag.Int("dns", 60, "dns queries to send")
	timeout := flag.Duration("timeout", 20*time.Second, "overall timeout")
	flag.Parse()
	_ = *timeout // per-phase deadlines below are tighter; kept for CLI symmetry

	if *secret == "" {
		fmt.Fprintln(os.Stderr, "need --secret")
		os.Exit(2)
	}
	relay, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", *host, *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	var sb [2]byte
	rand.Read(sb[:])
	session := binary.BigEndian.Uint16(sb[:])
	if session == 0 {
		session = 0x4242
	}
	fmt.Printf("relay=%v session=%04x links=%d\n", relay, session, *links)

	ls := make([]*link, 0, *links)
	for i := 0; i < *links; i++ {
		l, err := newLink(fmt.Sprintf("link%d", i), byte(i), relay)
		if err != nil {
			fmt.Fprintln(os.Stderr, "socket:", err)
			os.Exit(1)
		}
		if err := l.hello(session, *secret); err != nil {
			fmt.Fprintf(os.Stderr, "link %d HELLO failed: %v\n", i, err)
			os.Exit(1)
		}
		fmt.Printf("link %d registered via %s\n", i, l.conn.LocalAddr())
		ls = append(ls, l)
	}

	inbox := make(chan []byte, 8192)
	stop := make(chan struct{})
	defer close(stop)
	for _, l := range ls {
		go l.reader(inbox, stop)
	}

	// --- phase 1: ICMP echo through the tunnel
	var seqNo uint32 // one contiguous DATA sequence space across both phases
	icmpID := uint16(time.Now().UnixNano())
	icmpSent := map[uint16]bool{}
	for i := 0; i < *icmpCount; i++ {
		seq := uint16(i)
		icmpSent[seq] = true
		payload := make([]byte, 56)
		for j := range payload {
			payload[j] = byte(j)
		}
		pkt := ipv4Packet(1, clientIP, "1.1.1.1", icmpEcho(icmpID, seq, payload))
		ls[i%len(ls)].send(tData, session, uint32(i), pkt)
		seqNo++
	}
	icmpOK := countReplies(inbox, 4*time.Second, func(pkt []byte) bool {
		if len(pkt) < 28 || pkt[9] != 1 {
			return false
		}
		ihl := int(pkt[0]&0x0f) * 4
		if len(pkt) < ihl+8 {
			return false
		}
		if pkt[ihl] != 0 { // echo reply
			return false
		}
		id := binary.BigEndian.Uint16(pkt[ihl+4:])
		seq := binary.BigEndian.Uint16(pkt[ihl+6:])
		return id == icmpID && icmpSent[seq]
	})
	fmt.Printf("ICMP echo: %d/%d replied\n", icmpOK, *icmpCount)

	// --- phase 2: DNS lookups through the tunnel
	names := []string{"example.com", "cloudflare.com", "wikipedia.org", "github.com"}
	pending := make([]uint16, 0, *dnsCount)
	startDNS := time.Now()
	for i := 0; i < *dnsCount; i++ {
		txid := uint16(0x8000 + i)
		pending = append(pending, txid)
		q := dnsQuery(txid, names[i%len(names)], 1)
		sport := uint16(40000 + i%1000)
		pkt := ipv4Packet(17, clientIP, "1.1.1.1", udpSeg(sport, 53, q))
		ls[i%len(ls)].send(tData, session, seqNo, pkt)
		seqNo++
		if i%10 == 9 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	seen := map[uint16]bool{}
	want := map[uint16]bool{}
	for _, t := range pending {
		want[t] = true
	}
	dnsOK, dnsBytes := 0, 0
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && dnsOK < len(pending) {
		select {
		case pkt := <-inbox:
			txid, ok := dnsTxID(pkt)
			if !ok || !want[txid] || seen[txid] {
				continue
			}
			seen[txid] = true
			dnsOK++
			dnsBytes += len(pkt)
		case <-time.After(300 * time.Millisecond):
		}
	}
	elapsed := time.Since(startDNS)
	fmt.Printf("DNS: %d/%d answered, %d reply bytes in %.2fs\n", dnsOK, len(pending), dnsBytes, elapsed.Seconds())

	var txTotal, rxTotal int64
	for _, l := range ls {
		txTotal += l.tx
		rxTotal += l.rx
	}
	fmt.Printf("link bytes: tx=%d rx=%d\n", txTotal, rxTotal)
	for _, l := range ls {
		fmt.Printf("  %s %s tx=%d rx=%d\n", l.name, l.conn.LocalAddr(), l.tx, l.rx)
	}

	fail := 0
	if icmpOK < *icmpCount*9/10 {
		fmt.Printf("FAIL: only %d/%d icmp replies\n", icmpOK, *icmpCount)
		fail++
	}
	if dnsOK < len(pending)*9/10 {
		fmt.Printf("FAIL: only %d/%d dns answers\n", dnsOK, len(pending))
		fail++
	}
	if fail == 0 {
		fmt.Println("PASS: tunnel carries real internet traffic in both directions")
	}
	os.Exit(fail)
}

func countReplies(inbox <-chan []byte, d time.Duration, pred func([]byte) bool) int {
	n := 0
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		select {
		case pkt := <-inbox:
			if pred(pkt) {
				n++
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	return n
}

func dnsTxID(pkt []byte) (uint16, bool) {
	if len(pkt) < 28 {
		return 0, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if pkt[0]>>4 != 4 || pkt[9] != 17 || len(pkt) < ihl+8 {
		return 0, false
	}
	if binary.BigEndian.Uint16(pkt[ihl:]) != 53 { // source port must be DNS
		return 0, false
	}
	body := pkt[ihl+8:]
	if len(body) < 12 {
		return 0, false
	}
	return binary.BigEndian.Uint16(body), true
}
