//go:build linux

package relay

import (
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
)

// ExecRunner runs a real command and returns its combined output.
func ExecRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// SetupNAT builds and applies the host networking plan for --setup-nat.
func SetupNAT(cfg NATConfig, w io.Writer, log *slog.Logger) error {
	plan, err := BuildPlan(cfg)
	if err != nil {
		return err
	}
	log.Info("applying NAT plan", "wan", cfg.WANIface, "tun", cfg.TunIface, "client_cidr", cfg.ClientCIDR)
	if err := ApplyPlan(plan, ExecRunner, w); err != nil {
		return fmt.Errorf("%w\nplan:\n%s", err, strings.TrimSpace(plan.String()))
	}
	log.Info("NAT plan applied")
	return nil
}
