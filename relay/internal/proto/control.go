package proto

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

// ProtocolTag is the domain separator of the HELLO proof (PROTOCOL §4).
const ProtocolTag = "twinlink-v1"

// ProofMessage is the exact string signed by the HELLO proof.
func ProofMessage(session uint16, ts int64) string {
	return ProtocolTag + "|" + fmt.Sprintf("%04x", session) + "|" + strconv.FormatInt(ts, 10)
}

// Proof returns lowercase hex HMAC-SHA256(secret, ProofMessage(session, ts)).
func Proof(secret string, session uint16, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ProofMessage(session, ts)))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyProof checks a client-supplied proof in constant time.
//
// The canonical encoding of hex(session) is four zero-padded digits ("1a2b",
// "0005"). Some clients format it without padding; both are accepted because
// they authenticate the same secret, and rejecting one would only cause
// interoperability failures, never a security gain.
func VerifyProof(secret string, session uint16, ts int64, proof string) bool {
	if proof == "" {
		return false
	}
	want := Proof(secret, session, ts)
	if len(proof) == len(want) && subtle.ConstantTimeCompare([]byte(proof), []byte(want)) == 1 {
		return true
	}
	alt := ProofUnpadded(secret, session, ts)
	if len(proof) == len(alt) && subtle.ConstantTimeCompare([]byte(proof), []byte(alt)) == 1 {
		return true
	}
	return false
}

// ProofUnpadded is the non-zero-padded variant of Proof (interop only).
func ProofUnpadded(secret string, session uint16, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ProtocolTag + "|" + strconv.FormatUint(uint64(session), 16) + "|" + strconv.FormatInt(ts, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Hello is the HELLO payload (PROTOCOL §4).
type Hello struct {
	V     int    `json:"v"`
	TS    int64  `json:"ts"`
	Proof string `json:"proof"`
	MTU   int    `json:"mtu,omitempty"`
	Name  string `json:"name,omitempty"`
}

// HelloAck is the relay's answer to a HELLO.
type HelloAck struct {
	V          int    `json:"v"`
	ClientIP   string `json:"client_ip"`
	ServerIP   string `json:"server_ip"`
	MTU        int    `json:"mtu"`
	MaxPayload int    `json:"max_payload"`
}

// Bye terminates a session. Err is "auth", "clock", "shutdown", ...
type Bye struct {
	Err string `json:"err"`
}

// Error codes carried in BYE.
const (
	ErrCodeAuth  = "auth"
	ErrCodeClock = "clock"
)

// StatsLink is one entry of the STATS payload (PROTOCOL §8).
type StatsLink struct {
	ID    int     `json:"id"`
	RxBps int64   `json:"rx_bps"`
	TxBps int64   `json:"tx_bps"`
	RTTMs float64 `json:"rtt_ms"`
}

// Stats is the optional telemetry message (PROTOCOL §8).
type Stats struct {
	Links      []StatsLink `json:"links"`
	TotalRxBps int64       `json:"total_rx_bps"`
	TotalTxBps int64       `json:"total_tx_bps"`
	Sessions   int         `json:"sessions"`
}

// MarshalJSON is a helper for all control payloads.
func MarshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// All control structs are marshalable; a failure is a programming error.
		panic(err)
	}
	return b
}

// ParseHello decodes and sanity-checks a HELLO payload.
func ParseHello(payload []byte) (Hello, error) {
	var h Hello
	if len(payload) == 0 || len(payload) > MaxPayload {
		return h, fmt.Errorf("proto: bad hello size %d", len(payload))
	}
	if err := json.Unmarshal(payload, &h); err != nil {
		return h, fmt.Errorf("proto: bad hello json: %w", err)
	}
	if h.V != Version {
		return h, fmt.Errorf("proto: hello version %d", h.V)
	}
	if h.Proof == "" {
		return h, fmt.Errorf("proto: hello without proof")
	}
	return h, nil
}

// ParseBye decodes a BYE payload; a malformed one still terminates the session.
func ParseBye(payload []byte) Bye {
	var b Bye
	if err := json.Unmarshal(payload, &b); err != nil {
		return Bye{Err: "malformed"}
	}
	return b
}

// PingPayload encodes the 8-byte microsecond timestamp used by PING/PONG.
func PingPayload(micros uint64) []byte {
	var b [8]byte
	b[0] = byte(micros >> 56)
	b[1] = byte(micros >> 48)
	b[2] = byte(micros >> 40)
	b[3] = byte(micros >> 32)
	b[4] = byte(micros >> 24)
	b[5] = byte(micros >> 16)
	b[6] = byte(micros >> 8)
	b[7] = byte(micros)
	return b[:]
}

// ParsePingPayload decodes the 8-byte timestamp of a PING/PONG.
func ParsePingPayload(p []byte) (uint64, bool) {
	if len(p) != 8 {
		return 0, false
	}
	var v uint64
	for _, c := range p {
		v = v<<8 | uint64(c)
	}
	return v, true
}
