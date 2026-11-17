// Package relay implements the Twinlink relay: session table, per-link pumps,
// scheduling, reordering and the control HTTP server.
package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"twinlink/relay/internal/proto"
	"twinlink/relay/internal/sched"
	"twinlink/relay/internal/tunx"
)

// Protocol limits and defaults.
const (
	DefaultListenAddr  = ":51821"
	DefaultClientCIDR  = "10.99.0.0/24"
	DefaultServerIP    = "10.99.0.1"
	MaxLinksPerSession = 8
	DefaultSessionTTL  = 90 * time.Second
	DefaultPingEvery   = 1 * time.Second
	DefaultStatsEvery  = 2 * time.Second
	DefaultReorderTick = 10 * time.Millisecond
	DefaultQueueDepth  = 4
	DefaultIngressLen  = 8
	ClockSkewLimit     = 120 // seconds, PROTOCOL §4
)

// ClockSkewOK reports whether a HELLO timestamp is inside the +/-120 s window
// (PROTOCOL §4). The bound is inclusive: |now-ts| == 120 is accepted.
func ClockSkewOK(nowUnix, ts int64) bool {
	d := nowUnix - ts
	if d < 0 {
		d = -d
	}
	return d <= ClockSkewLimit
}

// Config configures an Engine.
type Config struct {
	// Secret is the shared HMAC key (>= 16 bytes). Never logged.
	Secret string
	// Tun is the tunnel device. Required.
	Tun tunx.Device
	// ListenAddr is the UDP listen address ("" means DefaultListenAddr).
	ListenAddr string
	// Logger receives structured logs (nil means discard).
	Logger *slog.Logger
	// ClientCIDR is the client pool ("" means 10.99.0.0/24).
	ClientCIDR string
	// ServerIP is the relay's tunnel address ("" means 10.99.0.1).
	ServerIP string
	// MTU is advertised in HELLO_ACK (0 means 1380).
	MTU int
	// QueueDepth is the per-link transmit queue length, in packets.
	QueueDepth int
	// SessionTTL is the idle timeout (0 means 90 s).
	SessionTTL time.Duration
	// PingEvery is the per-link PING interval (0 means 1 s).
	PingEvery time.Duration
	// StatsEvery is the STATS interval (0 disables STATS).
	StatsEvery time.Duration
	// ReorderTick is how often held packets are re-checked for timeout.
	ReorderTick time.Duration
	// WrapLinkWriter lets a caller decorate the per-link socket writer, e.g. to
	// emulate a WAN with added latency and a bandwidth cap. linkID is the
	// scheduler link id; next writes one datagram to the peer.
	WrapLinkWriter func(linkID uint8, next func([]byte) error) func([]byte) error
	// Now is the clock (nil means time.Now). It must be monotonic-friendly;
	// tests use the real clock because link shaping is wall-clock paced.
	Now func() time.Time
	// SchedOptions are passed to every session scheduler.
	SchedOptions []sched.Option
}

func (c *Config) normalize() error {
	if strings.TrimSpace(c.Secret) == "" {
		return errors.New("relay: empty secret")
	}
	if len(c.Secret) < 16 {
		return fmt.Errorf("relay: secret must be at least 16 bytes, got %d", len(c.Secret))
	}
	if c.Tun == nil {
		return errors.New("relay: nil TUN device")
	}
	if c.ListenAddr == "" {
		c.ListenAddr = DefaultListenAddr
	}
	if c.ClientCIDR == "" {
		c.ClientCIDR = DefaultClientCIDR
	}
	if c.ServerIP == "" {
		c.ServerIP = DefaultServerIP
	}
	if c.MTU <= 0 {
		c.MTU = proto.MTU
	}
	if c.QueueDepth <= 0 {
		c.QueueDepth = DefaultQueueDepth
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = DefaultSessionTTL
	}
	if c.PingEvery <= 0 {
		c.PingEvery = DefaultPingEvery
	}
	if c.ReorderTick <= 0 {
		c.ReorderTick = DefaultReorderTick
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	return nil
}

// Engine is the relay server.
type Engine struct {
	cfg      Config
	log      *slog.Logger
	now      func() time.Time
	tun      tunx.Device
	conn     *net.UDPConn
	cidr     *net.IPNet
	serverIP net.IP

	mu       sync.Mutex
	sessions map[uint16]*session
	addrIdx  map[string]*link
	ipIdx    map[string]*session
	ipUsed   map[byte]bool
	started  time.Time
	pingBase time.Time
	closed   bool

	counters Counters
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	controlLn  net.Listener
	controlSrv *http.Server
}

// New validates cfg and returns an engine that is not yet listening.
func New(cfg Config) (*Engine, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	_, cidr, err := net.ParseCIDR(cfg.ClientCIDR)
	if err != nil {
		return nil, fmt.Errorf("relay: bad client CIDR %q: %w", cfg.ClientCIDR, err)
	}
	ip := net.ParseIP(cfg.ServerIP).To4()
	if ip == nil {
		return nil, fmt.Errorf("relay: bad server IP %q", cfg.ServerIP)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		cfg:      cfg,
		log:      cfg.Logger,
		now:      cfg.Now,
		tun:      cfg.Tun,
		cidr:     cidr,
		serverIP: ip,
		sessions: make(map[uint16]*session),
		addrIdx:  make(map[string]*link),
		ipIdx:    make(map[string]*session),
		ipUsed:   make(map[byte]bool),
		ctx:      ctx,
		cancel:   cancel,
	}, nil
}

// Start binds the UDP socket and launches the data path.
func (e *Engine) Start() error {
	addr, err := net.ResolveUDPAddr("udp", e.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("relay: resolve %q: %w", e.cfg.ListenAddr, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("relay: listen %q: %w", e.cfg.ListenAddr, err)
	}
	if err := conn.SetReadBuffer(4 << 20); err != nil {
		e.log.Debug("udp read buffer", "err", err)
	}
	if err := conn.SetWriteBuffer(4 << 20); err != nil {
		e.log.Debug("udp write buffer", "err", err)
	}
	e.conn = conn
	e.started = e.now()
	// PING timestamps are microseconds on a *monotonic* base: the wall clock has
	// 0.5-15 ms granularity on Windows, which would quantise RTT to zero.
	e.pingBase = e.started

	e.wg.Add(3)
	go func() { defer e.wg.Done(); e.readLoop() }()
	go func() { defer e.wg.Done(); e.tunLoop() }()
	go func() { defer e.wg.Done(); e.maintainLoop() }()

	e.log.Info("relay listening",
		"udp", conn.LocalAddr().String(),
		"tun", e.tun.Name(),
		"client_cidr", e.cfg.ClientCIDR,
		"secret_bytes", len(e.cfg.Secret),
	)
	return nil
}

// ListenAddr returns the bound UDP address (useful with ":0" in tests).
func (e *Engine) ListenAddr() net.Addr {
	if e.conn == nil {
		return nil
	}
	return e.conn.LocalAddr()
}

// Tun returns the tunnel device.
func (e *Engine) Tun() tunx.Device { return e.tun }

// Close stops the engine.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	sessions := make([]*session, 0, len(e.sessions))
	for _, s := range e.sessions {
		sessions = append(sessions, s)
	}
	e.mu.Unlock()

	e.stopControl()
	e.cancel()
	if e.conn != nil {
		_ = e.conn.Close()
	}
	for _, s := range sessions {
		s.close("shutdown")
	}
	_ = e.tun.Close()
	e.wg.Wait()
	return nil
}

// ---------------------------------------------------------------- data paths

func (e *Engine) readLoop() {
	buf := make([]byte, proto.MaxDatagram+64)
	for {
		n, src, err := e.conn.ReadFromUDP(buf)
		if err != nil {
			if e.ctx.Err() != nil {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			e.log.Debug("udp read", "err", err)
			continue
		}
		e.counters.RXDatagrams.Add(1)
		e.counters.RXBytes.Add(uint64(n))
		// One allocation per datagram: the payload may outlive this iteration
		// inside the reorder buffer, so it must not alias a shared buffer.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		e.handleDatagram(pkt, src)
	}
}

// tunLoop moves packets from the tunnel to the session that owns the address.
func (e *Engine) tunLoop() {
	buf := make([]byte, proto.MTU+64)
	for {
		n, err := e.tun.ReadPacket(buf)
		if err != nil {
			if e.ctx.Err() != nil {
				return
			}
			if isClosedErr(err) {
				return
			}
			e.counters.TunReadErrors.Add(1)
			e.log.Debug("tun read", "err", err)
			continue
		}
		if n < 20 || !proto.IsIPv4(buf[:n]) || int(buf[0]&0x0f)*4 > n {
			e.counters.BadPayload.Add(1)
			continue
		}
		dst := net.IP(buf[16:20])
		e.mu.Lock()
		s := e.ipIdx[dst.String()]
		e.mu.Unlock()
		if s == nil {
			e.counters.NoRoute.Add(1)
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		// Backpressure: a full ingress queue means the links are saturated, so the
		// tunnel reader waits rather than dropping. If the session dies, give up.
		select {
		case s.ingress <- pkt:
		case <-s.done:
		case <-e.ctx.Done():
			return
		}
	}
}

func isClosedErr(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) ||
		strings.Contains(err.Error(), "file already closed") ||
		strings.Contains(err.Error(), "use of closed")
}

func (e *Engine) maintainLoop() {
	reorderTick := time.NewTicker(e.cfg.ReorderTick)
	defer reorderTick.Stop()
	pingTick := time.NewTicker(e.cfg.PingEvery)
	defer pingTick.Stop()
	var statsTick *time.Ticker
	var statsC <-chan time.Time
	if e.cfg.StatsEvery > 0 {
		statsTick = time.NewTicker(e.cfg.StatsEvery)
		statsC = statsTick.C
		defer statsTick.Stop()
	}

	for {
		select {
		case <-e.ctx.Done():
			return

		case <-reorderTick.C:
			for _, s := range e.sessionSnapshot() {
				s.flush(e.now())
			}
			e.reapExpired()

		case <-pingTick.C:
			now := e.now()
			for _, s := range e.sessionSnapshot() {
				s.sendPings(now)
			}

		case <-statsC:
			for _, s := range e.sessionSnapshot() {
				s.sendStats(e.statsForSession(s))
			}
		}
	}
}

// ---------------------------------------------------------------- dispatch

func (e *Engine) handleDatagram(d []byte, src *net.UDPAddr) {
	h, payload, err := proto.Parse(d)
	if err != nil {
		e.counters.Malformed.Add(1)
		return
	}
	key := src.String()

	e.mu.Lock()
	l := e.addrIdx[key]
	e.mu.Unlock()

	if l == nil {
		// PROTOCOL §4: unknown source addresses may only appear with a valid HELLO.
		if h.Type != proto.TypeHello {
			e.counters.UnknownSource.Add(1)
			return
		}
		e.handleHello(h, payload, src)
		return
	}

	s := l.sess
	if s.isClosed() {
		return
	}
	l.lastSeen.Store(e.now().UnixNano())
	s.sched.MarkAlive(l.id)

	switch h.Type {
	case proto.TypeData:
		e.handleData(l, h, payload)
	case proto.TypePing:
		e.sendPong(l, payload)
	case proto.TypePong:
		e.handlePong(l, payload)
	case proto.TypeHello:
		e.handleHello(h, payload, src)
	case proto.TypeBye:
		b := proto.ParseBye(payload)
		e.log.Info("client bye", "session", fmt.Sprintf("%04x", h.Session), "addr", key, "err", b.Err)
		s.close("client_bye")
		e.removeSession(s)
	case proto.TypeStats, proto.TypeHelloAck:
		// Telemetry from a client is optional and ignored by v1 relays.
	}
}

func (e *Engine) handleData(l *link, h proto.Header, payload []byte) {
	s := l.sess
	if !proto.IsIPv4(payload) {
		e.counters.BadPayload.Add(1)
		return
	}
	// Session isolation. Every session owns one address out of the pool; a packet claiming a
	// different source would be NATed as that address and its replies returned to whichever
	// session holds it. That is what happens when two clients both think they are 10.99.0.2,
	// so drop it loudly instead of quietly hijacking the other session's return traffic.
	if s.clientIP != nil {
		if want := s.clientIP.To4(); want != nil && !bytes.Equal(payload[12:16], want) {
			e.counters.ForeignSource.Add(1)
			return
		}
	}
	e.counters.DataIn.Add(1)
	l.rxBytes.Add(uint64(len(payload)))
	s.rxBytes.Add(uint64(len(payload)))

	now := e.now()
	s.mu.Lock()
	ok := s.rb.Push(h.Seq, payload, now)
	if !ok {
		e.counters.Duplicates.Add(1)
	}
	s.drainLocked(now)
	s.mu.Unlock()
	_ = ok
}

func (e *Engine) handlePong(l *link, payload []byte) {
	sentMicros, ok := proto.ParsePingPayload(payload)
	if !ok {
		e.counters.Malformed.Add(1)
		return
	}
	nowMicros := uint64(e.now().Sub(e.pingBase).Microseconds())
	if nowMicros < sentMicros {
		return
	}
	rtt := time.Duration(nowMicros-sentMicros) * time.Microsecond
	s := l.sess
	s.sched.ObserveRTT(l.id, rtt)
	s.mu.Lock()
	if s.rttMax < rtt {
		s.rttMax = rtt
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------- sessions

func (e *Engine) handleHello(h proto.Header, payload []byte, src *net.UDPAddr) {
	hello, err := proto.ParseHello(payload)
	if err != nil {
		e.counters.Malformed.Add(1)
		e.log.Debug("bad hello", "addr", src.String(), "err", err)
		return
	}
	now := e.now()
	if !ClockSkewOK(now.Unix(), hello.TS) {
		d := now.Unix() - hello.TS
		e.counters.ClockFailures.Add(1)
		e.log.Warn("hello clock skew", "addr", src.String(), "skew_s", d)
		e.sendBye(src, h.Session, proto.ErrCodeClock)
		return
	}
	if !proto.VerifyProof(e.cfg.Secret, h.Session, hello.TS, hello.Proof) {
		e.counters.AuthFailures.Add(1)
		e.log.Warn("hello auth failure", "addr", src.String(), "session", fmt.Sprintf("%04x", h.Session))
		e.sendBye(src, h.Session, proto.ErrCodeAuth)
		return
	}

	e.mu.Lock()
	s := e.sessions[h.Session]
	if s == nil {
		newS, err := e.newSessionLocked(h.Session)
		if err != nil {
			e.mu.Unlock()
			e.log.Warn("session refused", "session", fmt.Sprintf("%04x", h.Session), "err", err)
			e.sendBye(src, h.Session, "capacity")
			return
		}
		s = newS
	}
	l, created := e.addLinkLocked(s, src)
	e.mu.Unlock()

	if l == nil {
		e.counters.LinksRejected.Add(1)
		e.log.Warn("link refused", "session", fmt.Sprintf("%04x", h.Session), "addr", src.String(),
			"links", MaxLinksPerSession)
		return
	}
	s.touch()
	if created {
		e.log.Info("link up",
			"session", fmt.Sprintf("%04x", h.Session),
			"link", l.id,
			"addr", src.String(),
			"client_ip", s.clientIP.String(),
			"name", hello.Name,
		)
	}

	ack := proto.HelloAck{
		V:          proto.Version,
		ClientIP:   s.clientIP.String(),
		ServerIP:   e.serverIP.String(),
		MTU:        e.cfg.MTU,
		MaxPayload: proto.MaxPayload,
	}
	e.writeTo(src, proto.Header{Type: proto.TypeHelloAck, Session: s.id, LinkID: l.id}, proto.MarshalJSON(ack))
}

func (e *Engine) newSessionLocked(id uint16) (*session, error) {
	ip, ok := e.allocIPLocked()
	if !ok {
		return nil, errors.New("client address pool exhausted")
	}
	s := newSession(e, id, ip)
	e.sessions[id] = s
	e.ipIdx[ip.String()] = s
	e.ipUsed[ip[3]] = true
	e.counters.SessionsOpen.Add(1)
	e.counters.SessionsTotal.Add(1)
	s.start()
	e.wg.Add(1)
	go func() { defer e.wg.Done(); s.run() }()
	e.log.Info("session open", "session", fmt.Sprintf("%04x", id), "client_ip", ip.String())
	return s, nil
}

// allocIPLocked hands out 10.99.0.2 .. 10.99.0.254, lowest free address first.
//
// Lowest-first (rather than round-robin) means a client that reconnects after its
// session expired gets the same tunnel address back, which keeps the routing table
// and any firewall state predictable.
func (e *Engine) allocIPLocked() (net.IP, bool) {
	base := e.cidr.IP.To4()
	if base == nil {
		return nil, false
	}
	for last := 2; last <= 254; last++ {
		if e.ipUsed[byte(last)] {
			continue
		}
		ip := net.IPv4(base[0], base[1], base[2], byte(last)).To4()
		return ip, true
	}
	return nil, false
}

func (e *Engine) addLinkLocked(s *session, src *net.UDPAddr) (*link, bool) {
	key := src.String()
	if l, ok := e.addrIdx[key]; ok && l.sess == s {
		return l, false
	}
	l, ok := s.addLink(src)
	if !ok {
		return nil, false
	}
	e.addrIdx[key] = l
	return l, true
}

func (e *Engine) removeSession(s *session) {
	e.mu.Lock()
	if cur, ok := e.sessions[s.id]; ok && cur == s {
		delete(e.sessions, s.id)
	}
	delete(e.ipIdx, s.clientIP.String())
	delete(e.ipUsed, s.clientIP[3])
	for _, l := range s.links {
		delete(e.addrIdx, l.key)
	}
	if n := e.counters.SessionsOpen.Load(); n > 0 {
		e.counters.SessionsOpen.Add(^uint64(0)) // -1, atomics are unsigned
	}
	e.mu.Unlock()
}

func (e *Engine) reapExpired() {
	cutoff := e.now().Add(-e.cfg.SessionTTL)
	var dead []*session
	for _, s := range e.sessionSnapshot() {
		if s.lastActivity().Before(cutoff) {
			dead = append(dead, s)
		}
	}
	for _, s := range dead {
		e.log.Info("session expired", "session", fmt.Sprintf("%04x", s.id), "client_ip", s.clientIP.String())
		s.close("expired")
		e.removeSession(s)
	}
}

func (e *Engine) sessionSnapshot() []*session {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*session, 0, len(e.sessions))
	for _, s := range e.sessions {
		out = append(out, s)
	}
	return out
}

// ---------------------------------------------------------------- control TX

func (e *Engine) sendBye(addr *net.UDPAddr, session uint16, code string) {
	e.writeTo(addr, proto.Header{Type: proto.TypeBye, Session: session}, proto.MarshalJSON(proto.Bye{Err: code}))
}

func (e *Engine) sendPong(l *link, payload []byte) {
	if len(payload) != 8 {
		e.counters.Malformed.Add(1)
		return
	}
	e.writeTo(l.addr, proto.Header{Type: proto.TypePong, Session: l.sess.id, LinkID: l.id}, payload)
}

// writeTo sends one control datagram straight to the socket, bypassing the
// per-link transmit queue: control packets must not sit behind a data backlog.
func (e *Engine) writeTo(dst *net.UDPAddr, h proto.Header, payload []byte) {
	d := proto.Datagram(h, payload)
	if _, err := e.conn.WriteToUDP(d, dst); err != nil {
		e.counters.WriteErrors.Add(1)
		e.log.Debug("udp write", "addr", dst.String(), "err", err)
		return
	}
	e.counters.TXDatagrams.Add(1)
	e.counters.TXBytes.Add(uint64(len(d)))
}

// ---------------------------------------------------------------- stats

// Stats returns a snapshot for the control HTTP server.
func (e *Engine) Stats() EngineStats {
	sessions := e.sessionSnapshot()
	st := EngineStats{
		Sessions: len(sessions),
		Counters: e.counters.Snapshot(),
		Started:  e.started,
		UptimeS:  int64(e.now().Sub(e.started).Seconds()),
	}
	for _, s := range sessions {
		ss := e.statsForSession(s)
		st.Links += len(ss.Links)
		st.SessionsDetail = append(st.SessionsDetail, ss)
	}
	return st
}

func (e *Engine) statsForSession(s *session) SessionStats {
	now := e.now()
	out := SessionStats{
		ID:       int(s.id),
		ClientIP: s.clientIP.String(),
		IdleMs:   now.Sub(s.lastActivity()).Milliseconds(),
		RXBytes:  s.rxBytes.Load(),
		TXBytes:  s.txBytes.Load(),
	}
	rb := s.rb.Stats()
	out.Delivered = rb.Delivered
	out.Duplicate = rb.Duplicates
	out.Skipped = rb.Skipped
	for _, sn := range s.sched.Snapshot() {
		l := s.linkByID(sn.ID)
		ls := LinkStat{
			ID:      int(sn.ID),
			Alive:   sn.Alive,
			RTTMs:   float64(sn.RTT.Microseconds()) / 1000,
			RateBps: sn.RateBps,
			Weight:  sn.WeightBps,
		}
		if l != nil {
			ls.Addr = l.addr.String()
			ls.RXBytes = l.rxBytes.Load()
			ls.TXBytes = l.txBytes.Load()
		}
		out.Links = append(out.Links, ls)
	}
	return out
}
