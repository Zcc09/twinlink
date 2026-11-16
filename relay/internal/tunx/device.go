// Package tunx opens the relay's TUN device.
//
// Only the Linux implementation touches the kernel; on other platforms Open
// returns ErrUnsupported so that the logic packages (and their tests) still
// build and run everywhere.
package tunx

import (
	"errors"
	"time"
)

// ErrUnsupported is returned by Open on platforms without a TUN driver.
var ErrUnsupported = errors.New("tunx: TUN devices are only supported on linux")

// MTU is the tunnel MTU required by PROTOCOL §7.
const MTU = 1380

// DefaultName is the interface name the relay uses by default.
const DefaultName = "twinlink0"

// Device is a packet-oriented tunnel interface.
//
// ReadPacket blocks until one IP packet is available. Implementations must
// return a non-nil error once Close has been called.
type Device interface {
	// Name returns the interface name.
	Name() string
	// ReadPacket reads exactly one IP packet into buf and returns its length.
	ReadPacket(buf []byte) (int, error)
	// WritePacket writes exactly one IP packet.
	WritePacket(p []byte) (int, error)
	// Close shuts the device down.
	Close() error
}

// Config describes the device to open.
type Config struct {
	// Name is the interface name; empty means DefaultName.
	Name string
	// MTU is the interface MTU; zero means MTU.
	MTU int
	// ReadTimeout, when non-zero, makes ReadPacket return os.ErrDeadlineExceeded
	// instead of blocking forever.
	ReadTimeout time.Duration
}
