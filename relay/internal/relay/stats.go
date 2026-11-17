package relay

import (
	"sync/atomic"
	"time"
)

// Counters are the relay's lifetime counters, updated with atomics so the
// control HTTP server can read them without touching the data path's locks.
type Counters struct {
	RXDatagrams   atomic.Uint64
	TXDatagrams   atomic.Uint64
	RXBytes       atomic.Uint64
	TXBytes       atomic.Uint64
	DataIn        atomic.Uint64 // DATA datagrams accepted from clients
	DataOut       atomic.Uint64 // DATA datagrams handed to links
	Malformed     atomic.Uint64
	BadPayload    atomic.Uint64
	Duplicates    atomic.Uint64
	HolesSkipped  atomic.Uint64
	AuthFailures  atomic.Uint64
	ClockFailures atomic.Uint64
	UnknownSource atomic.Uint64
	Dropped       atomic.Uint64
	NoRoute       atomic.Uint64
	LinksRejected atomic.Uint64
	ForeignSource atomic.Uint64 // DATA dropped: source address not owned by that session
	SessionsOpen  atomic.Uint64
	SessionsTotal atomic.Uint64
	TunReadErrors atomic.Uint64
	WriteErrors   atomic.Uint64
}

// CounterSnapshot is the JSON-friendly copy returned by /stats.
type CounterSnapshot struct {
	RXDatagrams   uint64 `json:"rx_datagrams"`
	TXDatagrams   uint64 `json:"tx_datagrams"`
	RXBytes       uint64 `json:"rx_bytes"`
	TXBytes       uint64 `json:"tx_bytes"`
	DataIn        uint64 `json:"data_in"`
	DataOut       uint64 `json:"data_out"`
	Malformed     uint64 `json:"malformed"`
	BadPayload    uint64 `json:"bad_payload"`
	Duplicates    uint64 `json:"duplicates"`
	HolesSkipped  uint64 `json:"holes_skipped"`
	AuthFailures  uint64 `json:"auth_failures"`
	ClockFailures uint64 `json:"clock_failures"`
	UnknownSource uint64 `json:"unknown_source"`
	Dropped       uint64 `json:"dropped"`
	NoRoute       uint64 `json:"no_route"`
	LinksRejected uint64 `json:"links_rejected"`
	ForeignSource uint64 `json:"foreign_source"`
	SessionsTotal uint64 `json:"sessions_total"`
	TunReadErrors uint64 `json:"tun_read_errors"`
	WriteErrors   uint64 `json:"write_errors"`
}

// Snapshot copies every counter.
func (c *Counters) Snapshot() CounterSnapshot {
	return CounterSnapshot{
		RXDatagrams:   c.RXDatagrams.Load(),
		TXDatagrams:   c.TXDatagrams.Load(),
		RXBytes:       c.RXBytes.Load(),
		TXBytes:       c.TXBytes.Load(),
		DataIn:        c.DataIn.Load(),
		DataOut:       c.DataOut.Load(),
		Malformed:     c.Malformed.Load(),
		BadPayload:    c.BadPayload.Load(),
		Duplicates:    c.Duplicates.Load(),
		HolesSkipped:  c.HolesSkipped.Load(),
		AuthFailures:  c.AuthFailures.Load(),
		ClockFailures: c.ClockFailures.Load(),
		UnknownSource: c.UnknownSource.Load(),
		Dropped:       c.Dropped.Load(),
		NoRoute:       c.NoRoute.Load(),
		LinksRejected: c.LinksRejected.Load(),
		ForeignSource: c.ForeignSource.Load(),
		SessionsTotal: c.SessionsTotal.Load(),
		TunReadErrors: c.TunReadErrors.Load(),
		WriteErrors:   c.WriteErrors.Load(),
	}
}

// EngineStats is what GET /stats returns.
type EngineStats struct {
	Sessions       int             `json:"sessions"`
	Links          int             `json:"links"`
	Counters       CounterSnapshot `json:"counters"`
	UptimeS        int64           `json:"uptime_s"`
	Started        time.Time       `json:"started"`
	SessionsDetail []SessionStats  `json:"session_detail,omitempty"`
}

// SessionStats is the per-session part of GET /stats.
type SessionStats struct {
	ID        int        `json:"id"`
	ClientIP  string     `json:"client_ip"`
	Links     []LinkStat `json:"links"`
	IdleMs    int64      `json:"idle_ms"`
	RXBytes   uint64     `json:"rx_bytes"`
	TXBytes   uint64     `json:"tx_bytes"`
	Delivered uint64     `json:"delivered"`
	Duplicate uint64     `json:"duplicates"`
	Skipped   uint64     `json:"holes_skipped"`
}

// LinkStat describes one link inside a session.
type LinkStat struct {
	ID      int     `json:"id"`
	Addr    string  `json:"addr"`
	Alive   bool    `json:"alive"`
	RTTMs   float64 `json:"rtt_ms"`
	RateBps float64 `json:"rate_bps"`
	Weight  float64 `json:"weight_bps"`
	RXBytes uint64  `json:"rx_bytes"`
	TXBytes uint64  `json:"tx_bytes"`
}
