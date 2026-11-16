// Package proto implements the Twinlink v1 wire format (see docs/PROTOCOL.md).
//
// Everything here is pure Go: no sockets, no clocks, no OS calls, so the package
// tests run identically on every platform.
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// Version is the wire version carried in the high nibble of byte 0.
	Version = 1
	// HeaderLen is the fixed size of every datagram prefix.
	HeaderLen = 12
	// MaxPayload is the largest payload a v1 datagram may carry (PROTOCOL §2/§7).
	MaxPayload = 1400
	// MaxDatagram is the largest legal datagram: header + payload.
	MaxDatagram = HeaderLen + MaxPayload
	// MTU is the tunnel MTU on both ends (PROTOCOL §7).
	MTU = 1380
)

// Type is the low nibble of byte 0.
type Type uint8

const (
	TypeHello    Type = 1
	TypeHelloAck Type = 2
	TypeData     Type = 3
	TypePing     Type = 4
	TypePong     Type = 5
	TypeStats    Type = 6
	TypeBye      Type = 7
)

// String implements fmt.Stringer so logs read well.
func (t Type) String() string {
	switch t {
	case TypeHello:
		return "HELLO"
	case TypeHelloAck:
		return "HELLO_ACK"
	case TypeData:
		return "DATA"
	case TypePing:
		return "PING"
	case TypePong:
		return "PONG"
	case TypeStats:
		return "STATS"
	case TypeBye:
		return "BYE"
	default:
		return fmt.Sprintf("TYPE(%d)", uint8(t))
	}
}

// Valid reports whether t is a v1 message type.
func (t Type) Valid() bool { return t >= TypeHello && t <= TypeBye }

// Flags bits in byte 1.
const (
	FlagLastFrag uint8 = 1 << 0 // unused in v1, always 0 on send
	FlagAuth     uint8 = 1 << 1 // HELLO carries an HMAC proof
)

// Header is the 12-byte prefix present on every datagram.
type Header struct {
	Version  uint8
	Type     Type
	Flags    uint8
	Session  uint16
	Seq      uint32
	Length   uint16
	LinkID   uint8
	Reserved uint8
}

// Errors returned by Parse.
var (
	ErrShortDatagram   = errors.New("proto: datagram shorter than 12-byte header")
	ErrVersion         = errors.New("proto: unsupported version")
	ErrBadType         = errors.New("proto: unknown message type")
	ErrPayloadTooLarge = errors.New("proto: payload exceeds 1400 bytes")
	ErrLengthMismatch  = errors.New("proto: length field does not match datagram size")
	ErrBufferTooSmall  = errors.New("proto: destination buffer too small")
)

// Encode writes the header into the first HeaderLen bytes of dst.
func (h Header) Encode(dst []byte) {
	_ = dst[HeaderLen-1] // bounds check hint
	v := h.Version
	if v == 0 {
		v = Version
	}
	dst[0] = v<<4 | byte(h.Type)&0x0f
	dst[1] = h.Flags
	binary.BigEndian.PutUint16(dst[2:4], h.Session)
	binary.BigEndian.PutUint32(dst[4:8], h.Seq)
	binary.BigEndian.PutUint16(dst[8:10], h.Length)
	dst[10] = h.LinkID
	dst[11] = h.Reserved
}

// Parse validates b and returns the header plus a sub-slice of the payload.
func Parse(b []byte) (Header, []byte, error) {
	var h Header
	if len(b) < HeaderLen {
		return h, nil, ErrShortDatagram
	}
	h.Version = b[0] >> 4
	h.Type = Type(b[0] & 0x0f)
	h.Flags = b[1]
	h.Session = binary.BigEndian.Uint16(b[2:4])
	h.Seq = binary.BigEndian.Uint32(b[4:8])
	h.Length = binary.BigEndian.Uint16(b[8:10])
	h.LinkID = b[10]
	h.Reserved = b[11]

	if h.Version != Version {
		return h, nil, ErrVersion
	}
	if !h.Type.Valid() {
		return h, nil, ErrBadType
	}
	if h.Length > MaxPayload {
		return h, nil, ErrPayloadTooLarge
	}
	if int(h.Length) != len(b)-HeaderLen {
		return h, nil, ErrLengthMismatch
	}
	return h, b[HeaderLen:], nil
}

// AppendDatagram appends a complete datagram (header + payload) to dst.
func AppendDatagram(dst []byte, h Header, payload []byte) ([]byte, error) {
	if len(payload) > MaxPayload {
		return dst, ErrPayloadTooLarge
	}
	h.Length = uint16(len(payload))
	var hdr [HeaderLen]byte
	h.Encode(hdr[:])
	dst = append(dst, hdr[:]...)
	dst = append(dst, payload...)
	return dst, nil
}

// Datagram builds a fresh datagram. It panics on oversized payloads, which are
// programming errors (the MTU guarantees they cannot come from the tunnel).
func Datagram(h Header, payload []byte) []byte {
	b, err := AppendDatagram(make([]byte, 0, HeaderLen+len(payload)), h, payload)
	if err != nil {
		panic(err)
	}
	return b
}

// IsIPv4 reports whether p looks like an IPv4 packet (PROTOCOL §3/§7).
func IsIPv4(p []byte) bool {
	return len(p) > 0 && p[0]>>4 == 4
}
