// Command twinlink-relay runs the Twinlink bonding relay.
//
// It terminates a Twinlink UDP tunnel (PROTOCOL v1), reorders and exits client
// traffic to the internet through a TUN device, and spreads replies across all
// of the client's links in proportion to what each link can carry.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"twinlink/relay/internal/proto"
	"twinlink/relay/internal/relay"
	"twinlink/relay/internal/tunx"
)

// version is injected at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "twinlink-relay: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("twinlink-relay", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		listenAddr  = fs.String("listen", relay.DefaultListenAddr, "UDP listen address")
		httpAddr    = fs.String("http", relay.DefaultControlAddr, "control HTTP listen address (loopback by default)")
		tunName     = fs.String("tun", tunx.DefaultName, "TUN interface name")
		mtuFlag     = fs.Int("mtu", tunx.MTU, "TUN MTU")
		secretFlag  = fs.String("secret", "", "shared secret, >= 16 bytes (prefer the TWINLINK_SECRET env var)")
		clientCIDR  = fs.String("client-cidr", relay.DefaultClientCIDR, "client address pool")
		serverIP    = fs.String("server-ip", relay.DefaultServerIP, "relay tunnel address")
		sessionTTL  = fs.Duration("session-ttl", relay.DefaultSessionTTL, "session idle timeout")
		queueDepth  = fs.Int("queue-depth", relay.DefaultQueueDepth, "per-link transmit queue depth, in packets")
		wanIface    = fs.String("wan-iface", "eth0", "WAN interface used for NAT (--setup-nat)")
		setupNAT    = fs.Bool("setup-nat", false, "apply idempotent iptables/sysctl NAT rules, then serve")
		printNAT    = fs.Bool("print-nat", false, "print the NAT commands --setup-nat would run, then exit")
		logLevel    = fs.String("log-level", "info", "log level: debug, info, warn or error")
		noStats     = fs.Bool("no-stats", false, "do not emit STATS telemetry to clients")
		noHTTP      = fs.Bool("no-http", false, "do not serve the control HTTP API")
		showVersion = fs.Bool("version", false, "print the version and exit")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println("twinlink-relay " + version)
		return nil
	}

	logger, err := newLogger(*logLevel)
	if err != nil {
		return err
	}

	natCfg := relay.NATConfig{
		WANIface:   *wanIface,
		TunIface:   *tunName,
		ClientCIDR: *clientCIDR,
		ServerIP:   *serverIP,
		MTU:        *mtuFlag,
	}

	// --print-nat must work everywhere, including a Windows dev box with no
	// iptables: it only renders the plan.
	if *printNAT {
		plan, err := relay.BuildPlan(natCfg)
		if err != nil {
			return err
		}
		return relay.WritePlan(os.Stdout, plan)
	}

	secret := strings.TrimSpace(*secretFlag)
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("TWINLINK_SECRET"))
	}
	if secret == "" {
		return errors.New("no shared secret: set TWINLINK_SECRET or --secret")
	}
	if len(secret) < 16 {
		return fmt.Errorf("shared secret is %d bytes, the protocol requires at least 16", len(secret))
	}

	if *mtuFlag <= 0 || *mtuFlag > proto.MaxPayload {
		return fmt.Errorf("invalid MTU %d", *mtuFlag)
	}

	tun, err := tunx.Open(tunx.Config{Name: *tunName, MTU: *mtuFlag})
	if err != nil {
		return fmt.Errorf("open TUN: %w", err)
	}
	logger.Info("tunnel device ready", "name", tun.Name(), "mtu", *mtuFlag)

	if *setupNAT {
		if err := relay.SetupNAT(natCfg, os.Stdout, logger); err != nil {
			_ = tun.Close()
			return err
		}
	}

	cfg := relay.Config{
		Secret:     secret,
		Tun:        tun,
		ListenAddr: *listenAddr,
		Logger:     logger,
		ClientCIDR: *clientCIDR,
		ServerIP:   *serverIP,
		MTU:        *mtuFlag,
		QueueDepth: *queueDepth,
		SessionTTL: *sessionTTL,
		StatsEvery: relay.DefaultStatsEvery,
	}
	if *noStats {
		cfg.StatsEvery = 0
	}
	eng, err := relay.New(cfg)
	if err != nil {
		_ = tun.Close()
		return err
	}
	if err := eng.Start(); err != nil {
		_ = eng.Close()
		return err
	}
	if !*noHTTP {
		if err := eng.StartControl(*httpAddr); err != nil {
			_ = eng.Close()
			return err
		}
	}

	logger.Info("twinlink relay up",
		"version", version,
		"udp", *listenAddr,
		"http", *httpAddr,
		"client_cidr", *clientCIDR,
		"server_ip", *serverIP,
		"session_ttl", sessionTTL.String(),
	)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	s := <-sig
	logger.Info("shutting down", "signal", s.String())
	if err := eng.Close(); err != nil {
		return err
	}
	return nil
}

func newLogger(level string) (*slog.Logger, error) {
	var lv slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv = slog.LevelDebug
	case "", "info":
		lv = slog.LevelInfo
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", level)
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Timestamps are useful at info and below but noisy in containers.
			if a.Key == slog.TimeKey && lv >= slog.LevelInfo {
				a.Value = slog.StringValue(time.Now().UTC().Format("15:04:05.000"))
			}
			return a
		},
	})
	return slog.New(h), nil
}
