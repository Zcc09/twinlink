//go:build !linux

package relay

import (
	"errors"
	"io"
	"log/slog"
)

// ErrNATUnsupported is returned by SetupNAT off Linux.
var ErrNATUnsupported = errors.New("relay: --setup-nat requires linux (iptables/iproute2)")

// SetupNAT is unavailable off Linux, but the plan itself is not: --print-nat
// works everywhere because BuildPlan is pure.
func SetupNAT(cfg NATConfig, w io.Writer, log *slog.Logger) error { return ErrNATUnsupported }
