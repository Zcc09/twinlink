package relay_test

// Test harness for the relay: an in-memory TUN device, a token-bucket/latency
// emulator for a WAN link, and a minimal protocol client that speaks HELLO /
// PING / DATA over real loopback UDP sockets.
//
// The emulation is deliberately physical rather than logical:
//   * bandwidth is enforced by blocking the sender until tokens are available,
//     which is what a saturated socket does to a real writer, so the relay's
//     "bytes written this window" estimate measures capacity, not offered load;
//   * latency is applied to delivery (a timestamped release), not by sleeping
//     inside the write, so it does not serialise the link at 1/latency.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"twinlink/relay/internal/proto"
	"twinlink/relay/internal/relay"
	"twinlink/relay/internal/reorder"
)

// ---------------------------------------------------------------- fake TUN

type fakeTUN struct {
	in     chan []byte
	out    chan []byte
	closed chan struct{}
	once   sync.Once
	reads  atomic.Uint64
	writes atomic.Uint64
}

func newFakeTUN() *fakeTUN {
	return &fakeTUN{
		// Deliberately shallow: a deep tunnel queue is pure latency, and after a
		// host stall it turns into a long backlog that no 40 ms reorder hold can
		// absorb. The feeder blocks instead, which is what a real TUN does.
		in:     make(chan []byte, 4),
		out:    make(chan []byte, 8192),
		closed: make(chan struct{}),
	}
}

func (f *fakeTUN) Name() string { return "faketun0" }

func (f *fakeTUN) ReadPacket(b []byte) (int, error) {
	select {
	case p := <-f.in:
		f.reads.Add(1)
		return copy(b, p), nil
	case <-f.closed:
		return 0, net.ErrClosed
	}
}

func (f *fakeTUN) WritePacket(p []byte) (int, error) {
	f.writes.Add(1)
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case f.out <- cp:
	default:
		// Would only happen if a test stopped draining; fail loudly via the
		// channel length instead of silently losing a packet.
		return 0, errors.New("fakeTUN: output queue full")
	}
	return len(p), nil
}

func (f *fakeTUN) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// push injects one packet towards the relay's TUN reader (blocking, exactly
// like a real tunnel under backpressure).
func (f *fakeTUN) push(p []byte) bool {
	select {
	case f.in <- p:
		return true
	case <-f.closed:
		return false
	}
}

// ---------------------------------------------------------------- link sim

type delivery struct {
	data []byte
	due  time.Time
}

// shaper emulates one WAN link: a token bucket in bytes/s and a one-way delay.
type shaper struct {
	name    string
	rateBps float64
	rtt     time.Duration

	mu     sync.Mutex
	tokens float64
	burst  float64
	last   time.Time

	queue chan delivery
	stop  chan struct{}
	once  sync.Once

	next func([]byte) error

	bytesOut  atomic.Uint64
	pktsOut   atomic.Uint64
	writeErr  atomic.Uint64
	peakQ     atomic.Int64
	maxLate   atomic.Int64 // worst (actual write time - due time), ns
	maxWrite  atomic.Int64 // worst time spent inside the underlying UDP write
	maxGap    atomic.Int64 // worst spacing between consecutive writes(), ns
	lastWrite atomic.Int64
}

func newShaper(name string, rateBps float64, rtt time.Duration) *shaper {
	// Burst is 3 ms of capacity (at least two packets). It has to be at least a
	// few packets because the token refill is capped by the burst: with a 2-packet
	// (1.5 ms) bucket, the ~1 ms granularity of a Windows timer sleep throws away
	// most of the refill and an emulated 50 Mbit/s link tops out near 30 Mbit/s.
	burst := rateBps / 8 * 0.003
	if burst < 2*packetSize {
		burst = 2 * packetSize
	}
	return &shaper{
		name:    name,
		rateBps: rateBps,
		rtt:     rtt,
		tokens:  burst,
		burst:   burst,
		last:    time.Now(),
		queue:   make(chan delivery, 4096),
		stop:    make(chan struct{}),
	}
}

// attach starts delivering and returns the writer the relay will call.
// The RTT is emulated as one-way delay rtt/2 on this leg.
func (s *shaper) attach(next func([]byte) error) func([]byte) error {
	s.next = next
	go s.deliver()
	return s.write
}

func (s *shaper) write(b []byte) error {
	s.acquire(len(b))
	cp := make([]byte, len(b))
	copy(cp, b)
	now := time.Now()
	if prev := s.lastWrite.Load(); prev != 0 {
		if gap := now.UnixNano() - prev; gap > s.maxGap.Load() {
			s.maxGap.Store(gap)
		}
	}
	s.lastWrite.Store(now.UnixNano())
	d := delivery{data: cp, due: now.Add(s.rtt / 2)}
	select {
	case s.queue <- d:
		if n := int64(len(s.queue)); n > s.peakQ.Load() {
			s.peakQ.Store(n)
		}
	case <-s.stop:
		return net.ErrClosed
	}
	return nil
}

// acquire blocks until n bytes worth of tokens exist: this is the bandwidth cap
// and the source of backpressure.
func (s *shaper) acquire(n int) {
	need := float64(n)
	s.mu.Lock()
	for {
		now := time.Now()
		s.tokens += now.Sub(s.last).Seconds() * s.rateBps / 8
		if s.tokens > s.burst {
			s.tokens = s.burst
		}
		s.last = now
		if s.tokens >= need {
			s.tokens -= need
			s.mu.Unlock()
			return
		}
		wait := time.Duration((need - s.tokens) / (s.rateBps / 8) * float64(time.Second))
		if wait < 200*time.Microsecond {
			wait = 200 * time.Microsecond
		}
		s.mu.Unlock()
		time.Sleep(wait)
		s.mu.Lock()
		select {
		case <-s.stop:
			s.mu.Unlock()
			return
		default:
		}
	}
}

func (s *shaper) deliver() {
	for {
		select {
		case <-s.stop:
			return
		case d := <-s.queue:
			if w := time.Until(d.due); w > 0 {
				time.Sleep(w)
			}
			if late := int64(time.Since(d.due)); late > s.maxLate.Load() {
				s.maxLate.Store(late)
			}
			if s.next != nil {
				t0 := time.Now()
				err := s.next(d.data)
				if blk := int64(time.Since(t0)); blk > s.maxWrite.Load() {
					s.maxWrite.Store(blk)
				}
				if err == nil {
					s.bytesOut.Add(uint64(len(d.data)))
					s.pktsOut.Add(1)
				} else {
					s.writeErr.Add(1)
				}
			}
		}
	}
}

func (s *shaper) close() {
	s.once.Do(func() { close(s.stop) })
}

// ---------------------------------------------------------------- packets

const packetSize = proto.MTU // a full-MTU IPv4/UDP datagram

// makePacket builds a deterministic IPv4/UDP packet carrying its own index.
// Direction is internet -> client, i.e. exactly what the relay must read from
// its TUN and hand to the client.
func makePacket(i int) []byte {
	p := make([]byte, packetSize)
	p[0] = 0x45 // v4, IHL 5
	binary.BigEndian.PutUint16(p[2:4], uint16(packetSize))
	binary.BigEndian.PutUint16(p[4:6], uint16(i)) // ip id
	p[6] = 0x40                                   // DF
	p[8] = 64                                     // ttl
	p[9] = 17                                     // udp
	copy(p[12:16], net.IPv4(1, 1, 1, 1).To4())    // src: "internet"
	copy(p[16:20], net.IPv4(10, 99, 0, 2).To4())  // dst: the client
	binary.BigEndian.PutUint16(p[10:12], ipChecksum(p[:20]))

	binary.BigEndian.PutUint16(p[20:22], 443)
	binary.BigEndian.PutUint16(p[22:24], uint16(40000+i%1000))
	binary.BigEndian.PutUint16(p[24:26], uint16(packetSize-20))
	binary.BigEndian.PutUint32(p[28:32], uint32(i))
	for j := 32; j < packetSize; j++ {
		p[j] = byte(i*31 + j*7)
	}
	return p
}

func ipChecksum(hdr []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(hdr); i += 2 {
		sum += uint32(hdr[i])<<8 | uint32(hdr[i+1])
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

func packetIndex(p []byte) int {
	if len(p) < 32 {
		return -1
	}
	return int(binary.BigEndian.Uint32(p[28:32]))
}

// ---------------------------------------------------------------- client

type simSpec struct {
	RateBps float64
	RTT     time.Duration
}

type client struct {
	tb     testing.TB
	sess   uint16
	secret string
	socks  []*net.UDPConn
	shap   []*shaper // uplink shapers, one per socket (nil = unshaped)

	warmup int
	total  int
	pkts   [][]byte

	mu       sync.Mutex
	rb       *reorder.Buffer
	next     int
	problems []string
	skewMax  time.Duration
	lastArr  time.Time
	bySocket []atomic.Uint64 // measured-window bytes per socket
	counts   []atomic.Uint64 // measured-window packets per socket

	acks atomic.Int64
	done atomic.Int64
}

func newClient(tb testing.TB, sess uint16, secret string, warmup, total int) *client {
	c := &client{
		tb:     tb,
		sess:   sess,
		secret: secret,
		warmup: warmup,
		total:  total,
		rb:     reorder.New(reorder.WithStartSeq(0), reorder.WithFreezeGuard(reorder.DefaultFreezeGuard)),
		pkts:   make([][]byte, total),
	}
	for i := 0; i < total; i++ {
		c.pkts[i] = makePacket(i)
	}
	return c
}

// dial opens one UDP socket towards the relay and completes HELLO on it.
func (c *client) dial(addr *net.UDPAddr, name string) error {
	// Connected socket: one link per source port, and it can use Write/Read.
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return err
	}
	if err := conn.SetReadBuffer(32 << 20); err != nil {
		return err
	}
	if err := c.handshake(conn, name); err != nil {
		_ = conn.Close()
		return err
	}
	c.socks = append(c.socks, conn)
	c.bySocket = append(c.bySocket, atomic.Uint64{})
	c.counts = append(c.counts, atomic.Uint64{})
	return nil
}

func (c *client) handshake(conn *net.UDPConn, name string) error {
	ts := time.Now().Unix()
	hello := proto.Hello{V: proto.Version, TS: ts, Proof: proto.Proof(c.secret, c.sess, ts), MTU: proto.MTU, Name: name}
	d := proto.Datagram(proto.Header{
		Type: proto.TypeHello, Flags: proto.FlagAuth, Session: c.sess, Seq: 0,
	}, proto.MarshalJSON(hello))

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	buf := make([]byte, 2048)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := conn.Write(d); err != nil {
			return err
		}
		for {
			n, err := conn.Read(buf)
			if err != nil {
				break // deadline: retry the HELLO
			}
			h, payload, err := proto.Parse(buf[:n])
			if err != nil {
				continue
			}
			switch h.Type {
			case proto.TypeHelloAck:
				var ack proto.HelloAck
				if err := json.Unmarshal(payload, &ack); err != nil {
					return fmt.Errorf("bad hello_ack: %w", err)
				}
				if ack.V != proto.Version {
					return fmt.Errorf("hello_ack version %d", ack.V)
				}
				c.acks.Add(1)
				return nil
			case proto.TypeBye:
				return fmt.Errorf("relay said bye: %s", string(payload))
			case proto.TypePing:
				_, _ = conn.Write(proto.Datagram(proto.Header{Type: proto.TypePong, Session: c.sess, Seq: 0}, payload))
			}
		}
	}
	return errors.New("handshake timed out")
}

// readLoops starts one reader per socket.
func (c *client) readLoops() {
	for i, conn := range c.socks {
		go c.readLoop(i, conn)
	}
}

func (c *client) readLoop(sock int, conn *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		h, payload, err := proto.Parse(buf[:n])
		if err != nil {
			continue
		}
		switch h.Type {
		case proto.TypeData:
			idx := packetIndex(payload)
			if idx >= c.warmup {
				c.bySocket[sock].Add(uint64(len(payload)))
				c.counts[sock].Add(1)
			}
			cp := make([]byte, len(payload))
			copy(cp, payload)
			now := time.Now()
			c.mu.Lock()
			if !c.lastArr.IsZero() {
				if d := now.Sub(c.lastArr); d > c.skewMax {
					c.skewMax = d
				}
			}
			c.lastArr = now
			c.rb.PushAndDrain(h.Seq, cp, now, c.emitLocked)
			c.mu.Unlock()
		case proto.TypePing:
			d, err := proto.AppendDatagram(nil, proto.Header{Type: proto.TypePong, Session: c.sess, Seq: 0}, payload)
			if err == nil {
				_, _ = conn.Write(d)
			}
		case proto.TypeBye:
			c.tb.Logf("client %d: relay BYE %s", sock, string(payload))
		}
	}
}

// emitLocked verifies one packet delivered in order by the client's reorder
// buffer. c.mu must be held.
func (c *client) emitLocked(_ uint32, payload []byte) {
	idx := c.next
	if idx >= c.total {
		c.problems = append(c.problems, fmt.Sprintf("extra packet: index %d", packetIndex(payload)))
		return
	}
	pi := packetIndex(payload)
	if pi != idx {
		c.problems = append(c.problems, fmt.Sprintf("out of order: expected index %d, got %d", idx, pi))
		c.next = pi + 1
		return
	}
	if !bytes.Equal(payload, c.pkts[idx]) {
		c.problems = append(c.problems, fmt.Sprintf("content mismatch at index %d", idx))
	}
	c.next++
	if c.next == c.total {
		c.done.Store(1)
	}
}

func (c *client) close() {
	for _, s := range c.shap {
		if s != nil {
			s.close()
		}
	}
	for _, conn := range c.socks {
		_ = conn.Close()
	}
}

// ---------------------------------------------------------------- rig

type rig struct {
	tb     testing.TB
	engine *relay.Engine
	tun    *fakeTUN
	client *client
	shap   []*shaper
	addr   *net.UDPAddr
	specs  []simSpec
}

// newRig builds an engine with one link per spec, registering the links in
// order so that spec[i] gets scheduler link id i.
func newRig(tb testing.TB, specs []simSpec, warmup, total int, opts ...func(*relay.Config)) *rig {
	tb.Helper()
	tun := newFakeTUN()
	shapers := make([]*shaper, len(specs))
	for i, s := range specs {
		shapers[i] = newShaper(fmt.Sprintf("link%d", i), s.RateBps, s.RTT)
	}

	cfg := relay.Config{
		Secret:     testSecret,
		Tun:        tun,
		ListenAddr: "127.0.0.1:0",
		Logger:     testLogger(tb),
		// The product default. Depth is a real trade-off: every queued packet is
		// latency on the receiving end (2.2 ms per packet on a 5 Mbit/s link), but
		// a depth of 2 costs ~35% of a fast link's throughput because every
		// packet then needs its own goroutine hand-off.
		QueueDepth:  relay.DefaultQueueDepth,
		StatsEvery:  2 * time.Second,
		PingEvery:   200 * time.Millisecond,
		ReorderTick: 5 * time.Millisecond,
		WrapLinkWriter: func(id uint8, next func([]byte) error) func([]byte) error {
			if int(id) < len(shapers) {
				return shapers[id].attach(next)
			}
			return next
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	eng, err := relay.New(cfg)
	if err != nil {
		tb.Fatalf("relay.New: %v", err)
	}
	if err := eng.Start(); err != nil {
		tb.Fatalf("engine start: %v", err)
	}
	addr := eng.ListenAddr().(*net.UDPAddr)

	cl := newClient(tb, 0x2a2a, testSecret, warmup, total)
	for i := range specs {
		if err := cl.dial(addr, fmt.Sprintf("sim%d", i)); err != nil {
			_ = eng.Close()
			tb.Fatalf("client dial %d: %v", i, err)
		}
	}
	cl.readLoops()

	return &rig{tb: tb, engine: eng, tun: tun, client: cl, shap: shapers, addr: addr, specs: specs}
}

func (r *rig) close() {
	r.client.close()
	_ = r.engine.Close()
	for _, s := range r.shap {
		s.close()
	}
	_ = r.tun.Close()
}

type runResult struct {
	Elapsed time.Duration
	Bytes   int
	// MeasuredElapsed is the time from the last warm-up packet to the last
	// measured packet: the window the split and the goodput are reported over.
	MeasuredElapsed time.Duration
	MeasuredBytes   int
	Delivered       int
	ByLink          []uint64
	MaxSkew         time.Duration
	Problems        []string
	Duplicates      uint64
	Skipped         uint64
	MaxHold         time.Duration
	RbStats         reorder.Stats
	Stats           relay.EngineStats
	Diag            string
}

// run pushes c.total packets into the tunnel as fast as the pipeline accepts
// them, waits for every one of them to be delivered in order, and reports what
// happened.
func (r *rig) run(timeout time.Duration) runResult {
	r.tb.Helper()
	start := time.Now()
	feederDone := make(chan struct{})
	go func() {
		defer close(feederDone)
		for _, p := range r.client.pkts {
			if !r.tun.push(p) {
				return
			}
		}
	}()

	deadline := time.Now().Add(timeout)
	var measureStart time.Time
	for r.client.done.Load() == 0 {
		if measureStart.IsZero() {
			r.client.mu.Lock()
			n := r.client.next
			r.client.mu.Unlock()
			if n >= r.client.warmup {
				measureStart = time.Now()
			}
		}
		if time.Now().After(deadline) {
			r.client.mu.Lock()
			n := r.client.next
			r.client.mu.Unlock()
			r.tb.Fatalf("timed out after %s: delivered %d/%d packets", timeout, n, r.client.total)
		}
		time.Sleep(2 * time.Millisecond)
	}
	elapsed := time.Since(start)
	if measureStart.IsZero() {
		measureStart = start
	}
	<-feederDone

	r.client.mu.Lock()
	defer r.client.mu.Unlock()
	res := runResult{
		Elapsed:         elapsed,
		Bytes:           r.client.total * packetSize,
		MeasuredElapsed: elapsed - measureStart.Sub(start),
		MeasuredBytes:   (r.client.total - r.client.warmup) * packetSize,
		Delivered:       r.client.next,
		MaxSkew:         r.client.skewMax,
		Problems:        append([]string(nil), r.client.problems...),
		Duplicates:      r.client.rb.Stats().Duplicates,
		Skipped:         r.client.rb.Stats().Skipped,
		MaxHold:         r.client.rb.Stats().MaxHold,
		RbStats:         r.client.rb.Stats(),
	}
	for i := range r.client.bySocket {
		res.ByLink = append(res.ByLink, r.client.bySocket[i].Load())
	}
	res.Stats = r.engine.Stats()
	var diag []string
	for i, s := range r.shap {
		diag = append(diag, fmt.Sprintf("shaper%d(rtt=%v,cap=%.1fMbit/s): %d pkts %d B out, %d write errors, peak queue %d, max late %v, max enqueue gap %v, max udp write block %v",
			i, s.rtt, s.rateBps/1e6, s.pktsOut.Load(), s.bytesOut.Load(), s.writeErr.Load(), s.peakQ.Load(),
			time.Duration(s.maxLate.Load()), time.Duration(s.maxGap.Load()), time.Duration(s.maxWrite.Load())))
	}
	res.Diag = joinStrings(diag, "\n  ")
	return res
}
