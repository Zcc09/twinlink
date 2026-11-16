package relay

import (
	"fmt"
	"io"
	"strings"
)

// Runner executes one command and returns its combined output.
type Runner func(name string, args ...string) (string, error)

// ApplyPlan performs the plan through run, checking each rule before adding it,
// so running it twice changes nothing the second time.
//
// It is pure Go (the command runner is injected), which is why the exact same
// logic can be unit-tested on Windows while `--setup-nat` on Linux passes an
// os/exec runner.
func ApplyPlan(p Plan, run Runner, w io.Writer) error {
	if run == nil {
		return fmt.Errorf("relay: nil runner")
	}
	for _, c := range p.Commands {
		if err := applyOne(c, run, w); err != nil {
			return err
		}
	}
	return nil
}

func applyOne(c Command, run Runner, w io.Writer) error {
	label := c.Description
	if label == "" {
		label = shellJoin(c.Apply)
	}

	// sysctl: the probe reports a value, not a presence.
	if len(c.Probe) >= 2 && c.Probe[0] == "sysctl" && c.Probe[1] == "-n" {
		out, err := run(c.Probe[0], c.Probe[1:]...)
		if err == nil && strings.TrimSpace(out) == "1" {
			fmt.Fprintf(w, "= %s: already set\n", label)
			return nil
		}
		if _, err := run(c.Apply[0], c.Apply[1:]...); err != nil {
			return fmt.Errorf("relay: %s: %w", label, err)
		}
		fmt.Fprintf(w, "+ %s: %s\n", label, shellJoin(c.Apply))
		return nil
	}

	// iproute2 verbs used here are declarative (replace/set), so they are simply
	// applied; the probe is only there to make --print-nat readable.
	if c.IgnoreProbeError {
		if _, err := run(c.Apply[0], c.Apply[1:]...); err != nil {
			return fmt.Errorf("relay: %s: %w", label, err)
		}
		fmt.Fprintf(w, "+ %s: %s\n", label, shellJoin(c.Apply))
		return nil
	}

	if len(c.Probe) > 0 {
		if _, err := run(c.Probe[0], c.Probe[1:]...); err == nil {
			fmt.Fprintf(w, "= %s: already present\n", label)
			return nil
		}
	}
	if _, err := run(c.Apply[0], c.Apply[1:]...); err != nil {
		return fmt.Errorf("relay: %s: %w", label, err)
	}
	fmt.Fprintf(w, "+ %s: %s\n", label, shellJoin(c.Apply))

	// Verify: a rule we just added must be visible to the same check that said
	// it was missing. Trusting the add call alone has bitten enough people.
	if len(c.Probe) > 0 {
		if out, err := run(c.Probe[0], c.Probe[1:]...); err != nil {
			return fmt.Errorf("relay: %s: rule not present after add (%s): %w", label, strings.TrimSpace(out), err)
		}
	}
	return nil
}
