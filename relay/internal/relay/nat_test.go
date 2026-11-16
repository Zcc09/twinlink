package relay_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"twinlink/relay/internal/relay"
)

func TestBuildNATPlan(t *testing.T) {
	plan, err := relay.BuildPlan(relay.NATConfig{
		WANIface:   "eth0",
		TunIface:   "twinlink0",
		ClientCIDR: "10.99.0.0/24",
		ServerIP:   "10.99.0.1",
		MTU:        1380,
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	out := plan.String()
	t.Logf("plan:\n%s", out)

	want := []string{
		"sysctl -w net.ipv4.ip_forward=1",
		"ip link set dev twinlink0 mtu 1380",
		"ip addr replace 10.99.0.1/24 dev twinlink0",
		"ip link set dev twinlink0 up",
		"iptables -t nat -C POSTROUTING -s 10.99.0.0/24 -o eth0 -j MASQUERADE || " +
			"iptables -t nat -A POSTROUTING -s 10.99.0.0/24 -o eth0 -j MASQUERADE",
		"iptables -C FORWARD -i twinlink0 -o eth0 -j ACCEPT || " +
			"iptables -A FORWARD -i twinlink0 -o eth0 -j ACCEPT",
		"iptables -C FORWARD -i eth0 -o twinlink0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT || " +
			"iptables -A FORWARD -i eth0 -o twinlink0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("plan is missing %q", w)
		}
	}
	if len(plan.Commands) != 7 {
		t.Errorf("plan has %d commands, want 7", len(plan.Commands))
	}

	// Every rule must be checked with -C before it is added: that is what makes
	// --setup-nat idempotent.
	for _, c := range plan.Commands {
		if len(c.Apply) == 0 {
			t.Fatalf("command %q has no apply step", c.Description)
		}
		if c.Apply[0] == "iptables" && len(c.Probe) == 0 {
			t.Errorf("iptables rule %q has no -C probe", c.Description)
		}
	}
}

func TestBuildNATPlanRequiresWAN(t *testing.T) {
	if _, err := relay.BuildPlan(relay.NATConfig{}); !errors.Is(err, relay.ErrNoWANIface) {
		t.Fatalf("err = %v, want ErrNoWANIface", err)
	}
}

func TestWritePlan(t *testing.T) {
	plan, err := relay.BuildPlan(relay.NATConfig{WANIface: "wan0"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := relay.WritePlan(&buf, plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "-o wan0") {
		t.Errorf("--print-nat output does not mention the WAN interface:\n%s", buf.String())
	}
}

// fakeSystem emulates iptables/ip/sysctl closely enough to prove ApplyPlan is
// idempotent and that it verifies what it added.
type fakeSystem struct {
	forward bool
	rules   map[string]bool
	calls   [][]string
	noopAdd bool // pretend -A does nothing (to test verification)
}

func newFakeSystem() *fakeSystem { return &fakeSystem{rules: map[string]bool{}} }

func (f *fakeSystem) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	switch name {
	case "sysctl":
		if len(args) == 2 && args[0] == "-n" {
			if f.forward {
				return "1\n", nil
			}
			return "0\n", nil
		}
		if len(args) == 2 && args[0] == "-w" && args[1] == "net.ipv4.ip_forward=1" {
			f.forward = true
			return "net.ipv4.ip_forward = 1\n", nil
		}
		return "", errors.New("bad sysctl")
	case "ip":
		return "", nil
	case "iptables":
		op := ""
		for _, a := range args {
			if a == "-C" || a == "-A" {
				op = a
			}
		}
		key := ruleKey(args)
		switch op {
		case "-C":
			if f.rules[key] {
				return "", nil
			}
			return "", errors.New("iptables: Bad rule (does a matching rule exist in that chain?)")
		case "-A":
			if !f.noopAdd {
				f.rules[key] = true
			}
			return "", nil
		}
	}
	return "", errors.New("unexpected command " + name)
}

// ruleKey normalises a rule so that -C and -A produce the same key.
func ruleKey(args []string) string {
	k := make([]string, len(args))
	copy(k, args)
	for i, a := range k {
		if a == "-C" || a == "-A" {
			k[i] = "@OP"
		}
	}
	return strings.Join(k, " ")
}

func (f *fakeSystem) adds() int {
	n := 0
	for _, c := range f.calls {
		for _, a := range c {
			if a == "-A" {
				n++
			}
		}
	}
	return n
}

func TestApplyPlanIsIdempotent(t *testing.T) {
	plan, err := relay.BuildPlan(relay.NATConfig{WANIface: "eth0", TunIface: "twinlink0"})
	if err != nil {
		t.Fatal(err)
	}
	sys := newFakeSystem()

	var first bytes.Buffer
	if err := relay.ApplyPlan(plan, sys.run, &first); err != nil {
		t.Fatalf("first apply: %v\n%s", err, first.String())
	}
	firstAdds := sys.adds()
	if firstAdds != 3 {
		t.Fatalf("first apply added %d rules, want 3 (masquerade + 2 forward)\n%s", firstAdds, first.String())
	}

	var second bytes.Buffer
	if err := relay.ApplyPlan(plan, sys.run, &second); err != nil {
		t.Fatalf("second apply: %v\n%s", err, second.String())
	}
	if got := sys.adds() - firstAdds; got != 0 {
		t.Fatalf("second apply added %d rules, want 0\n%s", got, second.String())
	}
	if !strings.Contains(second.String(), "already present") {
		t.Errorf("second apply did not report existing rules:\n%s", second.String())
	}
	if !strings.Contains(second.String(), "already set") {
		t.Errorf("second apply did not report ip_forward as set:\n%s", second.String())
	}
	if !sys.forward {
		t.Error("ip_forward was not enabled")
	}
}

func TestApplyPlanVerifiesAdds(t *testing.T) {
	plan, err := relay.BuildPlan(relay.NATConfig{WANIface: "eth0"})
	if err != nil {
		t.Fatal(err)
	}
	sys := newFakeSystem()
	sys.noopAdd = true // iptables silently accepts -A but the rule never lands

	var out bytes.Buffer
	err = relay.ApplyPlan(plan, sys.run, &out)
	if err == nil {
		t.Fatalf("ApplyPlan reported success although nothing was added\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "not present after add") {
		t.Errorf("err = %v, want a verification failure", err)
	}
}
