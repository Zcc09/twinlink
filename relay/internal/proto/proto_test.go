package proto_test

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"twinlink/relay/internal/proto"
)

// TestHeaderRoundTrip covers every message type: encode then decode must return
// exactly the same header and payload.
func TestHeaderRoundTrip(t *testing.T) {
	types := []proto.Type{
		proto.TypeHello, proto.TypeHelloAck, proto.TypeData,
		proto.TypePing, proto.TypePong, proto.TypeStats, proto.TypeBye,
	}
	cases := []struct {
		name    string
		session uint16
		seq     uint32
		linkID  uint8
		flags   uint8
		payload []byte
	}{
		{"empty", 1, 0, 0, 0, nil},
		{"one byte", 0xffff, 1, 0, 0, []byte{4}},
		{"hello json", 0x1a2b, 0, 0, proto.FlagAuth, []byte(`{"v":1,"ts":1737000000,"proof":"deadbeef"}`)},
		{"max payload", 0x1234, 0xffffffff, 7, 0, make([]byte, proto.MaxPayload)},
		{"mtu payload", 0x0002, 0x80000000, 3, 0, make([]byte, 1380)},
	}
	for _, ty := range types {
		for _, c := range cases {
			t.Run(ty.String()+"/"+c.name, func(t *testing.T) {
				h := proto.Header{
					Type:    ty,
					Flags:   c.flags,
					Session: c.session,
					Seq:     c.seq,
					LinkID:  c.linkID,
				}
				for i := range c.payload {
					c.payload[i] = byte(i)
				}
				d, err := proto.AppendDatagram(nil, h, c.payload)
				if err != nil {
					t.Fatalf("AppendDatagram: %v", err)
				}
				if len(d) != proto.HeaderLen+len(c.payload) {
					t.Fatalf("datagram is %d bytes, want %d", len(d), proto.HeaderLen+len(c.payload))
				}
				got, payload, err := proto.Parse(d)
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if got.Version != proto.Version {
					t.Errorf("version = %d", got.Version)
				}
				if got.Type != ty {
					t.Errorf("type = %v, want %v", got.Type, ty)
				}
				if got.Session != c.session || got.Seq != c.seq || got.LinkID != c.linkID || got.Flags != c.flags {
					t.Errorf("header mismatch: got %+v", got)
				}
				if got.Length != uint16(len(c.payload)) {
					t.Errorf("length = %d, want %d", got.Length, len(c.payload))
				}
				if len(payload) != len(c.payload) {
					t.Fatalf("payload is %d bytes, want %d", len(payload), len(c.payload))
				}
				for i := range payload {
					if payload[i] != byte(i) {
						t.Fatalf("payload byte %d = %d", i, payload[i])
					}
				}
			})
		}
	}
}

// TestWireLayout pins the on-the-wire byte layout of PROTOCOL §2.
func TestWireLayout(t *testing.T) {
	d := proto.Datagram(proto.Header{
		Type: proto.TypeData, Session: 0x1a2b, Seq: 0x01020304, LinkID: 7,
	}, []byte{0x45, 0x00})
	want := []byte{
		0x13,       // version 1, type 3
		0x00,       // flags
		0x1a, 0x2b, // session
		0x01, 0x02, 0x03, 0x04, // seq
		0x00, 0x02, // length
		0x07, // link id
		0x00, // reserved
		0x45, 0x00,
	}
	if len(d) != len(want) {
		t.Fatalf("datagram is %d bytes, want %d", len(d), len(want))
	}
	for i := range want {
		if d[i] != want[i] {
			t.Fatalf("byte %d = 0x%02x, want 0x%02x (datagram %x)", i, d[i], want[i], d)
		}
	}
}

func TestParseRejectsShortDatagrams(t *testing.T) {
	for n := 0; n < proto.HeaderLen; n++ {
		if _, _, err := proto.Parse(make([]byte, n)); err != proto.ErrShortDatagram {
			t.Fatalf("%d-byte datagram: err = %v, want ErrShortDatagram", n, err)
		}
	}
}

func TestParseRejectsLengthMismatch(t *testing.T) {
	// header claims 100 payload bytes, datagram carries 99.
	d := proto.Datagram(proto.Header{Type: proto.TypeData, Length: 99}, make([]byte, 99))
	binary.BigEndian.PutUint16(d[8:10], 100)
	if _, _, err := proto.Parse(d); err != proto.ErrLengthMismatch {
		t.Fatalf("err = %v, want ErrLengthMismatch", err)
	}
	// header claims 98, datagram carries 99.
	binary.BigEndian.PutUint16(d[8:10], 98)
	if _, _, err := proto.Parse(d); err != proto.ErrLengthMismatch {
		t.Fatalf("err = %v, want ErrLengthMismatch", err)
	}
	// header claims 0, datagram carries 99.
	binary.BigEndian.PutUint16(d[8:10], 0)
	if _, _, err := proto.Parse(d); err != proto.ErrLengthMismatch {
		t.Fatalf("err = %v, want ErrLengthMismatch", err)
	}
}

func TestParseRejectsOversizedPayload(t *testing.T) {
	d := make([]byte, proto.HeaderLen+proto.MaxPayload+1)
	d[0] = 0x13
	binary.BigEndian.PutUint16(d[8:10], proto.MaxPayload+1)
	if _, _, err := proto.Parse(d); err != proto.ErrPayloadTooLarge {
		t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
	}
	// 1400 is legal.
	d = make([]byte, proto.HeaderLen+proto.MaxPayload)
	d[0] = 0x13
	binary.BigEndian.PutUint16(d[8:10], proto.MaxPayload)
	if _, _, err := proto.Parse(d); err != nil {
		t.Fatalf("1400-byte payload rejected: %v", err)
	}
}

func TestParseRejectsVersionAndType(t *testing.T) {
	d := proto.Datagram(proto.Header{Type: proto.TypeData}, nil)
	d[0] = 0x23 // version 2
	if _, _, err := proto.Parse(d); err != proto.ErrVersion {
		t.Errorf("version 2: err = %v, want ErrVersion", err)
	}
	d = proto.Datagram(proto.Header{Type: proto.TypeData}, nil)
	d[0] = 0x10 // version 1, type 0
	if _, _, err := proto.Parse(d); err != proto.ErrBadType {
		t.Errorf("type 0: err = %v, want ErrBadType", err)
	}
	d = proto.Datagram(proto.Header{Type: proto.TypeData}, nil)
	d[0] = 0x19 // version 1, type 9
	if _, _, err := proto.Parse(d); err != proto.ErrBadType {
		t.Errorf("type 9: err = %v, want ErrBadType", err)
	}
}

func TestAppendDatagramRejectsOversize(t *testing.T) {
	if _, err := proto.AppendDatagram(nil, proto.Header{Type: proto.TypeData}, make([]byte, proto.MaxPayload+1)); err != proto.ErrPayloadTooLarge {
		t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
	}
}

// TestHMACVectors is the interoperability gate: these two proofs are fixed in
// PROTOCOL §4/§5 and any change to the canonical message breaks the Android
// client.
func TestHMACVectors(t *testing.T) {
	const secret = "twinlink-test-secret"
	cases := []struct {
		ts   int64
		want string
	}{
		{1737000000, "6ef1b4cbb185afd7ed31c37926e9ffda827911cc178389e564b72afae14fb7fc"},
		{1737000060, "cbb14259f7bc39a2971758cee8accfeae8960ee0457fa8dc0986cee0d6327a40"},
	}
	for _, c := range cases {
		if got, want := proto.ProofMessage(0x1a2b, c.ts), fmt.Sprintf("twinlink-v1|1a2b|%d", c.ts); got != want {
			t.Fatalf("ProofMessage = %q", got)
		}
		got := proto.Proof(secret, 0x1a2b, c.ts)
		if got != c.want {
			t.Fatalf("Proof(0x1a2b, %d) = %s, want %s", c.ts, got, c.want)
		}
		if !proto.VerifyProof(secret, 0x1a2b, c.ts, got) {
			t.Fatalf("VerifyProof rejected its own proof")
		}
		if proto.VerifyProof(secret+"x", 0x1a2b, c.ts, got) {
			t.Fatalf("VerifyProof accepted the wrong secret")
		}
		if proto.VerifyProof(secret, 0x1a2c, c.ts, got) {
			t.Fatalf("VerifyProof accepted the wrong session")
		}
		if proto.VerifyProof(secret, 0x1a2b, c.ts+1, got) {
			t.Fatalf("VerifyProof accepted the wrong timestamp")
		}
	}
	// The proof is lowercase hex of 32 bytes.
	p := proto.Proof(secret, 1, 1)
	if len(p) != 64 || hex.EncodeToString([]byte(p)) != p && strings.ToLower(p) != p {
		t.Fatalf("proof %q is not 64 lowercase hex chars", p)
	}
}

func TestProofFormatIsPaddedHexSession(t *testing.T) {
	cases := []struct {
		session uint16
		want    string
	}{
		{0x0001, "twinlink-v1|0001|1712345678"},
		{0x00ff, "twinlink-v1|00ff|1712345678"},
		{0x1a2b, "twinlink-v1|1a2b|1712345678"},
		{0xffff, "twinlink-v1|ffff|1712345678"},
	}
	for _, c := range cases {
		if got := proto.ProofMessage(c.session, 1712345678); got != c.want {
			t.Errorf("ProofMessage(%#04x) = %q, want %q", c.session, got, c.want)
		}
	}
	// Interop: a client that renders the session without zero padding is accepted.
	unpadded := proto.ProofUnpadded("twinlink-test-secret", 5, 1737000000)
	if !proto.VerifyProof("twinlink-test-secret", 5, 1737000000, unpadded) {
		t.Error("unpadded hex(session) proof rejected")
	}
	if proto.VerifyProof("twinlink-test-secret", 5, 1737000000, "") {
		t.Error("empty proof accepted")
	}
}

func TestControlJSONRoundTrip(t *testing.T) {
	h := proto.Hello{V: 1, TS: 1737000000, Proof: "abc", MTU: 1380, Name: "pixel-wifi"}
	got, err := proto.ParseHello(proto.MarshalJSON(h))
	if err != nil {
		t.Fatalf("ParseHello: %v", err)
	}
	if got != h {
		t.Errorf("hello round trip: %+v != %+v", got, h)
	}

	for _, bad := range [][]byte{
		nil,
		[]byte(`not json`),
		[]byte(`{"v":2,"ts":1,"proof":"x"}`),
		[]byte(`{"v":1,"ts":1}`),
	} {
		if _, err := proto.ParseHello(bad); err == nil {
			t.Errorf("ParseHello accepted %q", string(bad))
		}
	}

	ack := proto.HelloAck{V: 1, ClientIP: "10.99.0.2", ServerIP: "10.99.0.1", MTU: 1380, MaxPayload: 1400}
	var back proto.HelloAck
	if err := json.Unmarshal(proto.MarshalJSON(ack), &back); err != nil {
		t.Fatal(err)
	}
	if back != ack {
		t.Errorf("hello_ack round trip: %+v != %+v", back, ack)
	}
	for _, key := range []string{"client_ip", "server_ip", "mtu", "max_payload", "v"} {
		if !strings.Contains(string(proto.MarshalJSON(ack)), `"`+key+`"`) {
			t.Errorf("hello_ack JSON lacks %q: %s", key, proto.MarshalJSON(ack))
		}
	}

	bye := proto.Bye{Err: proto.ErrCodeAuth}
	if got := proto.ParseBye(proto.MarshalJSON(bye)); got.Err != proto.ErrCodeAuth {
		t.Errorf("bye round trip: %+v", got)
	}
	if got := proto.ParseBye([]byte("garbage")); got.Err == "" {
		t.Error("malformed BYE produced an empty error code")
	}

	st := proto.Stats{Sessions: 1, TotalTxBps: 234567, Links: []proto.StatsLink{{ID: 0, RxBps: 123456, RTTMs: 21.4}}}
	s := string(proto.MarshalJSON(st))
	for _, key := range []string{"links", "rx_bps", "tx_bps", "rtt_ms", "total_rx_bps", "total_tx_bps", "sessions"} {
		if !strings.Contains(s, `"`+key+`"`) {
			t.Errorf("stats JSON lacks %q: %s", key, s)
		}
	}
}

func TestPingPayloadRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 1737000000000000, ^uint64(0)} {
		p := proto.PingPayload(v)
		if len(p) != 8 {
			t.Fatalf("ping payload is %d bytes, want 8", len(p))
		}
		got, ok := proto.ParsePingPayload(p)
		if !ok || got != v {
			t.Fatalf("round trip %d -> %d (ok=%v)", v, got, ok)
		}
	}
	if _, ok := proto.ParsePingPayload([]byte{1, 2, 3}); ok {
		t.Error("3-byte ping payload accepted")
	}
}

func TestIsIPv4(t *testing.T) {
	if !proto.IsIPv4([]byte{0x45, 0, 0}) {
		t.Error("0x45 not recognized as IPv4")
	}
	for _, p := range [][]byte{nil, {0x60}, {0x00}, {0xff}} {
		if proto.IsIPv4(p) {
			t.Errorf("%x recognized as IPv4", p)
		}
	}
}
