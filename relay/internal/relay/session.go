package relay

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"twinlink/relay/internal/proto"
	"twinlink/relay/internal/reorder"
	"twinlink/relay/internal/sched"
)

// link is one registered source address of a session.
type link struct {
	id       uint8
	key      string
	addr     *net.UDPAddr
	sess     *session
	write    func([]byte) error
	q        chan []byte
	lastSeen atomic.Int64 // unix nanos
	rxBytes  atomic.Uint64
	txBytes  atomic.Uint64
}

// sendable reports whether the link's transmit queue has room. It is called by
// the scheduler while choosing a link; with a single producer per link the
// answer cannot go stale before the send.
func (l *link) sendable() bool { return len(l.q) < cap(l.q) }

// session is one authenticated tunnel endpoint group.
type session struct {
	e        *Engine
	id       uint16
	clientIP net.IP

	sched   *sched.Scheduler
	rb      *reorder.Buffer
	ingress chan []byte
	done    chan struct{}
	spaceCh chan struct{}

	started   bool
	closeOnce sync.Once
	closedFlg atomic.Bool

	mu          sync.Mutex
	links       []*link // indexed by id
	nextLinkID  uint8
	txSeq       uint32
	rttMax      time.Duration
	lastSkipped uint64

	lastAct  atomic.Int64
	rxBytes  atomic.Uint64
	txBytes  atomic.Uint64
	tunBytes atomic.Uint64
	pings    atomic.Uint64
}

func newSession(e *Engine, id uint16, clientIP net.IP) *session {
	opts := append([]sched.Option{sched.WithClock(e.cfg.Now)}, e.cfg.SchedOptions...)
	s := &session{
		e:        e,
		id:       id,
		clientIP: clientIP,
		sched:    sched.New(opts...),
		rb: reorder.New(
			reorder.WithClock(e.cfg.Now),
			reorder.WithStartSeq(0), // PROTOCOL §2: DATA starts at 0
			reorder.WithFreezeGuard(reorder.DefaultFreezeGuard),
		),
		ingress: make(chan []byte, DefaultIngressLen),
		done:    make(chan struct{}),
		spaceCh: make(chan struct{}, 1),
		links:   make([]*link, 0, MaxLinksPerSession),
	}
	s.touch()
	return s
}

func (s *session) start() { s.started = true }

func (s *session) isClosed() bool { return s.closedFlg.Load() }

func (s *session) touch() { s.lastAct.Store(s.e.now().UnixNano()) }

func (s *session) lastActivity() time.Time { return time.Unix(0, s.lastAct.Load()) }

func (s *session) linkByID(id uint8) *link {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		if l.id == id {
			return l
		}
	}
	return nil
}

// addLink registers a new source address. Called with the engine lock held.
// It returns nil when the session already holds the protocol maximum.
func (s *session) addLink(addr *net.UDPAddr) (*link, bool) {
	s.mu.Lock()
	if len(s.links) >= MaxLinksPerSession {
		s.mu.Unlock()
		return nil, false
	}
	id := s.nextLinkID
	s.nextLinkID++
	key := addr.String()

	next := func(b []byte) error {
		_, err := s.e.conn.WriteToUDP(b, addr)
		return err
	}
	write := next
	if s.e.cfg.WrapLinkWriter != nil {
		write = s.e.cfg.WrapLinkWriter(id, next)
	}
	l := &link{
		id:    id,
		key:   key,
		addr:  addr,
		sess:  s,
		write: write,
		q:     make(chan []byte, s.e.cfg.QueueDepth),
	}
	l.lastSeen.Store(s.e.now().UnixNano())
	s.links = append(s.links, l)
	s.sched.Add(id)
	s.sched.MarkAlive(id)
	s.mu.Unlock()

	go l.pump(context.Background(), s)
	return l, true
}

// pump drains one link's transmit queue. A blocking write is intentional: it is
// what turns "bytes written this window" into a measurement of link capacity.
func (l *link) pump(_ context.Context, s *session) {
	for {
		select {
		case <-s.done:
			return
		case d := <-l.q:
			if err := l.write(d); err != nil {
				s.e.counters.WriteErrors.Add(1)
				s.e.log.Debug("link write", "link", l.id, "err", err)
			} else {
				l.txBytes.Add(uint64(len(d)))
				s.txBytes.Add(uint64(len(d)))
				s.sched.AddSentBytes(l.id, len(d))
				s.e.counters.DataOut.Add(1)
				s.e.counters.TXDatagrams.Add(1)
				s.e.counters.TXBytes.Add(uint64(len(d)))
			}
			select {
			case s.spaceCh <- struct{}{}:
			default:
			}
		}
	}
}

// run is the session's scheduler goroutine: it moves packets from the tunnel
// ingress queue onto links, blocking while the links are saturated.
func (s *session) run() {
	for {
		select {
		case pkt := <-s.ingress:
			if !s.enqueue(pkt) {
				return
			}
		case <-s.done:
			return
		case <-s.e.ctx.Done():
			return
		}
	}
}

// enqueue schedules one IP packet onto a link, waiting for capacity when every
// link is full. It returns false once the session is gone.
func (s *session) enqueue(pkt []byte) bool {
	bits := float64(len(pkt)+proto.HeaderLen) * 8
	sendable := func(id uint8) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if int(id) >= len(s.links) {
			return false
		}
		return s.links[id].sendable()
	}

	stalls := 0
	for {
		id, ok := s.sched.Pick(bits, sendable)
		if ok {
			s.mu.Lock()
			l := s.links[id]
			seq := s.txSeq
			s.txSeq++
			d := proto.Datagram(proto.Header{
				Type:    proto.TypeData,
				Session: s.id,
				Seq:     seq,
				LinkID:  id,
			}, pkt)
			s.mu.Unlock()
			select {
			case l.q <- d:
				return true
			case <-s.done:
				return false
			}
		}
		stalls++
		if stalls > 64 {
			// Nothing has been able to take data for ~1 s: nobody is listening.
			s.e.counters.Dropped.Add(1)
			return true
		}
		// Backoff, not a signal: a wake-up channel loses notifications when both
		// links free a slot at once, and a lost wake-up stalls the whole tunnel
		// for the length of the timeout. A short poll keeps that bounded (and the
		// fallthrough is what detects dead links).
		select {
		case <-time.After(time.Millisecond):
		case <-s.done:
			return false
		case <-s.e.ctx.Done():
			return false
		}
	}
}

// drainLocked releases reorderable packets to the TUN. s.mu must be held.
func (s *session) drainLocked(now time.Time) {
	s.rb.Drain(now, func(_ uint32, p []byte) {
		if _, err := s.e.tun.WritePacket(p); err != nil {
			s.e.counters.WriteErrors.Add(1)
			return
		}
		s.tunBytes.Add(uint64(len(p)))
	})
	st := s.rb.Stats()
	if st.Skipped > s.lastSkipped {
		s.e.counters.HolesSkipped.Add(st.Skipped - s.lastSkipped)
		s.lastSkipped = st.Skipped
	}
}

// flush releases packets whose hole has aged out, even without new arrivals.
func (s *session) flush(now time.Time) {
	if s.isClosed() {
		return
	}
	s.mu.Lock()
	s.drainLocked(now)
	s.mu.Unlock()
}

// sendPings emits one PING per link (PROTOCOL §5 liveness + RTT measurement).
func (s *session) sendPings(now time.Time) {
	if s.isClosed() {
		return
	}
	// Monotonic microseconds since the relay started (PROTOCOL §3 asks for a
	// monotonic-ish epoch); the echo is subtracted from the same base.
	payload := proto.PingPayload(uint64(now.Sub(s.e.pingBase).Microseconds()))
	s.mu.Lock()
	links := make([]*link, len(s.links))
	copy(links, s.links)
	s.mu.Unlock()
	for _, l := range links {
		s.e.writeTo(l.addr, proto.Header{Type: proto.TypePing, Session: s.id, LinkID: l.id}, payload)
		s.pings.Add(1)
	}
}

// sendStats emits the optional telemetry message on every link.
func (s *session) sendStats(st SessionStats) {
	if s.isClosed() {
		return
	}
	msg := proto.Stats{Sessions: 1}
	for _, l := range st.Links {
		msg.Links = append(msg.Links, proto.StatsLink{
			ID:    l.ID,
			RxBps: int64(l.RateBps),
			TxBps: int64(l.RateBps),
			RTTMs: l.RTTMs,
		})
		msg.TotalRxBps += int64(l.RateBps)
	}
	payload := proto.MarshalJSON(msg)
	s.mu.Lock()
	links := make([]*link, len(s.links))
	copy(links, s.links)
	s.mu.Unlock()
	for _, l := range links {
		s.e.writeTo(l.addr, proto.Header{Type: proto.TypeStats, Session: s.id, LinkID: l.id}, payload)
	}
}

// close tears the session down once. It does not touch the engine maps; the
// caller removes the session (removeSession) to keep the lock order simple.
func (s *session) close(reason string) {
	s.closeOnce.Do(func() {
		s.closedFlg.Store(true)
		close(s.done)
		if reason != "" && reason != "client_bye" {
			func() {
				defer func() { _ = recover() }()
				s.sendByeToAll(reason)
			}()
		}
		s.e.log.Debug("session closed", "session", fmt.Sprintf("%04x", s.id), "reason", reason)
	})
}

func (s *session) sendByeToAll(reason string) {
	payload := proto.MarshalJSON(proto.Bye{Err: reason})
	s.mu.Lock()
	links := make([]*link, len(s.links))
	copy(links, s.links)
	s.mu.Unlock()
	for _, l := range links {
		s.e.writeTo(l.addr, proto.Header{Type: proto.TypeBye, Session: s.id}, payload)
	}
}
