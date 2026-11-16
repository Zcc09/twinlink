package relay

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// NATConfig describes the host networking the relay needs.
type NATConfig struct {
	// WANIface is the interface that faces the internet (e.g. eth0).
	WANIface string
	// TunIface is the relay's tunnel interface (e.g. twinlink0).
	TunIface string
	// ClientCIDR is the pool that must be masqueraded (e.g. 10.99.0.0/24).
	ClientCIDR string
	// ServerIP is the relay's tunnel address, configured on TunIface.
	ServerIP string
	// MTU is configured on TunIface (0 means 1380).
	MTU int
}

// Command is one idempotent change: Probe must succeed with exit status 0 when
// the change is already in place; Apply performs it.
type Command struct {
	// Description is a short human label ("masquerade clients").
	Description string
	// Probe is the check (e.g. iptables -C ...).
	Probe []string
	// Apply is the mutation (e.g. iptables -A ...).
	Apply []string
	// IgnoreProbeError treats a failing probe as "not applied" instead of "error".
	IgnoreProbeError bool
}

// Plan is the complete, ordered set of changes --setup-nat makes.
type Plan struct {
	Commands []Command
}

// ErrNoWANIface is returned by BuildPlan when the WAN interface is unknown.
var ErrNoWANIface = errors.New("relay: NAT requires a WAN interface (--wan-iface)")

// BuildPlan derives the idempotent firewall/routing changes from cfg.
//
// It is pure: no command runs here, which is what makes --print-nat useful on a
// machine (like a Windows dev box) that has no iptables at all.
func BuildPlan(cfg NATConfig) (Plan, error) {
	wan := strings.TrimSpace(cfg.WANIface)
	if wan == "" {
		return Plan{}, ErrNoWANIface
	}
	tun := strings.TrimSpace(cfg.TunIface)
	if tun == "" {
		tun = "twinlink0"
	}
	cidr := strings.TrimSpace(cfg.ClientCIDR)
	if cidr == "" {
		cidr = DefaultClientCIDR
	}
	server := strings.TrimSpace(cfg.ServerIP)
	if server == "" {
		server = DefaultServerIP
	}
	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = 1380
	}

	var cmds []Command

	// 1. IPv4 forwarding.
	cmds = append(cmds, Command{
		Description:      "enable IPv4 forwarding",
		Probe:            []string{"sysctl", "-n", "net.ipv4.ip_forward"},
		Apply:            []string{"sysctl", "-w", "net.ipv4.ip_forward=1"},
		IgnoreProbeError: true,
	})

	// 2. Bring the tunnel interface up with its address and MTU.
	cmds = append(cmds, Command{
		Description:      "set tunnel MTU",
		Probe:            []string{"ip", "-o", "link", "show", "dev", tun, "mtu", fmt.Sprint(mtu)},
		Apply:            []string{"ip", "link", "set", "dev", tun, "mtu", fmt.Sprint(mtu)},
		IgnoreProbeError: true,
	})
	cmds = append(cmds, Command{
		Description:      "assign tunnel address",
		Probe:            []string{"ip", "-o", "addr", "show", "dev", tun},
		Apply:            []string{"ip", "addr", "replace", server + "/24", "dev", tun},
		IgnoreProbeError: true,
	})
	cmds = append(cmds, Command{
		Description:      "bring tunnel interface up",
		Probe:            []string{"ip", "-o", "link", "show", "dev", tun, "up"},
		Apply:            []string{"ip", "link", "set", "dev", tun, "up"},
		IgnoreProbeError: true,
	})

	// 3. Masquerade client traffic leaving the WAN interface.
	masq := []string{"iptables", "-t", "nat", "-C", "POSTROUTING", "-s", cidr, "-o", wan, "-j", "MASQUERADE"}
	cmds = append(cmds, Command{
		Description: "masquerade client subnet out of " + wan,
		Probe:       masq,
		Apply:       []string{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", cidr, "-o", wan, "-j", "MASQUERADE"},
	})

	// 4. Forward client traffic out of the WAN interface.
	cmds = append(cmds, Command{
		Description: "forward tunnel to " + wan,
		Probe:       []string{"iptables", "-C", "FORWARD", "-i", tun, "-o", wan, "-j", "ACCEPT"},
		Apply:       []string{"iptables", "-A", "FORWARD", "-i", tun, "-o", wan, "-j", "ACCEPT"},
	})

	// 5. Forward established replies back into the tunnel.
	cmds = append(cmds, Command{
		Description: "forward established replies back to tunnel",
		Probe: []string{"iptables", "-C", "FORWARD", "-i", wan, "-o", tun,
			"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		Apply: []string{"iptables", "-A", "FORWARD", "-i", wan, "-o", tun,
			"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	})

	return Plan{Commands: cmds}, nil
}

// ShellLines renders the plan as the exact shell commands --setup-nat runs,
// using the classic "check, else add" idiom so the output is self-documenting.
func (p Plan) ShellLines() []string {
	out := make([]string, 0, len(p.Commands))
	for _, c := range p.Commands {
		probe := shellJoin(c.Probe)
		apply := shellJoin(c.Apply)
		switch {
		case len(c.Probe) == 0:
			out = append(out, apply)
		case c.IgnoreProbeError && c.Probe[0] == "sysctl":
			out = append(out, apply)
		case c.Probe[0] == "ip":
			out = append(out, "# "+probe+"   # "+c.Description+"\n"+apply)
		default:
			out = append(out, probe+" || "+apply)
		}
	}
	return out
}

// String renders the plan as one printable block.
func (p Plan) String() string {
	var b strings.Builder
	b.WriteString("# twinlink-relay --setup-nat plan (idempotent: every rule is checked before it is added)\n")
	for _, line := range p.ShellLines() {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func shellJoin(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\$`|&;<>()*?[]{}!") {
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}

// WritePlan prints the plan (used by --print-nat).
func WritePlan(w io.Writer, p Plan) error {
	_, err := io.WriteString(w, p.String())
	return err
}
