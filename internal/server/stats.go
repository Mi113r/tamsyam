package server

import (
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mi113r/tamsyam/internal/store"
)

const (
	statsFlushInterval = 10 * time.Second
	statsRetention     = 30 * 24 * time.Hour
	maxPathLen         = 200
	liveWindow         = 1000 // recent requests kept for rpm and percentiles
)

// sample is one finished HTTP request (status > 0) or TCP connection (status == 0).
type sample struct {
	tunnel            string
	at                time.Time
	status            int
	durMs             int64
	bytesIn, bytesOut int64
	remoteIP          string
	path              string
}

// Collector aggregates samples in memory and periodically adds them to the store.
type Collector struct {
	st  *store.Store
	log *slog.Logger

	mu     sync.Mutex
	deltas map[store.StatKey]*store.StatDelta
	ips    map[store.DayKey]int64
	paths  map[store.DayKey]int64

	stop chan struct{}
	done chan struct{}
}

func newCollector(st *store.Store, log *slog.Logger) *Collector {
	c := &Collector{st: st, log: log, stop: make(chan struct{}), done: make(chan struct{})}
	c.reset()
	return c
}

func (c *Collector) reset() {
	c.deltas = map[store.StatKey]*store.StatDelta{}
	c.ips = map[store.DayKey]int64{}
	c.paths = map[store.DayKey]int64{}
}

func (c *Collector) record(s sample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := store.StatKey{Tunnel: s.tunnel, Minute: s.at.Unix() / 60}
	d := c.deltas[k]
	if d == nil {
		d = &store.StatDelta{}
		c.deltas[k] = d
	}
	if s.status == 0 {
		d.Conns++
	} else {
		d.Requests++
		d.DurMs += s.durMs
		switch s.status / 100 {
		case 2:
			d.S2xx++
		case 3:
			d.S3xx++
		case 4:
			d.S4xx++
		case 5:
			d.S5xx++
		}
	}
	d.BytesIn += s.bytesIn
	d.BytesOut += s.bytesOut

	day := s.at.Unix() / 86400
	if s.remoteIP != "" {
		c.ips[store.DayKey{Tunnel: s.tunnel, Day: day, Value: s.remoteIP}]++
	}
	if s.path != "" {
		if len(s.path) > maxPathLen {
			s.path = s.path[:maxPathLen]
		}
		c.paths[store.DayKey{Tunnel: s.tunnel, Day: day, Value: s.path}]++
	}
}

// Flush writes pending counters to the store.
func (c *Collector) Flush() error {
	c.mu.Lock()
	deltas, ips, paths := c.deltas, c.ips, c.paths
	c.reset()
	c.mu.Unlock()
	if len(deltas) == 0 && len(ips) == 0 && len(paths) == 0 {
		return nil
	}
	return c.st.AddStats(deltas, ips, paths)
}

func (c *Collector) run() {
	defer close(c.done)
	flush := time.NewTicker(statsFlushInterval)
	defer flush.Stop()
	purge := time.NewTicker(time.Hour)
	defer purge.Stop()
	for {
		select {
		case <-flush.C:
			if err := c.Flush(); err != nil {
				c.log.Error("flush stats", "err", err)
			}
		case <-purge.C:
			if err := c.st.PurgeStats(time.Now().Add(-statsRetention)); err != nil {
				c.log.Error("purge stats", "err", err)
			}
		case <-c.stop:
			if err := c.Flush(); err != nil {
				c.log.Error("flush stats", "err", err)
			}
			return
		}
	}
}

func (c *Collector) close() {
	close(c.stop)
	<-c.done
}

// LiveStats are the in-memory counters of a connected tunnel.
type LiveStats struct {
	Requests    int64 `json:"requests"`
	RPM         int   `json:"rpm"`
	P50Ms       int64 `json:"p50_ms"`
	P90Ms       int64 `json:"p90_ms"`
	S4xx        int64 `json:"s4xx"`
	S5xx        int64 `json:"s5xx"`
	ActiveConns int64 `json:"active_conns"`
	TotalConns  int64 `json:"total_conns"`
	BytesIn     int64 `json:"bytes_in"`
	BytesOut    int64 `json:"bytes_out"`
}

type liveStats struct {
	mu       sync.Mutex
	recent   []recentReq // ring buffer of the last liveWindow requests
	next     int
	requests int64
	s4xx     int64
	s5xx     int64

	activeConns, totalConns atomic.Int64
	bytesIn, bytesOut       atomic.Int64
}

type recentReq struct {
	at    time.Time
	durMs int64
}

func (l *liveStats) addRequest(at time.Time, status int, durMs, in, out int64) {
	l.bytesIn.Add(in)
	l.bytesOut.Add(out)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests++
	switch status / 100 {
	case 4:
		l.s4xx++
	case 5:
		l.s5xx++
	}
	r := recentReq{at: at, durMs: durMs}
	if len(l.recent) < liveWindow {
		l.recent = append(l.recent, r)
	} else {
		l.recent[l.next] = r
		l.next = (l.next + 1) % liveWindow
	}
}

func (l *liveStats) snapshot() LiveStats {
	s := LiveStats{
		ActiveConns: l.activeConns.Load(), TotalConns: l.totalConns.Load(),
		BytesIn: l.bytesIn.Load(), BytesOut: l.bytesOut.Load(),
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s.Requests, s.S4xx, s.S5xx = l.requests, l.s4xx, l.s5xx

	now := time.Now()
	durs := make([]int64, 0, len(l.recent))
	for _, r := range l.recent {
		if now.Sub(r.at) <= time.Minute {
			s.RPM++
		}
		if now.Sub(r.at) <= 5*time.Minute {
			durs = append(durs, r.durMs)
		}
	}
	if len(durs) > 0 {
		slices.Sort(durs)
		s.P50Ms = durs[len(durs)*50/100]
		s.P90Ms = durs[min(len(durs)*90/100, len(durs)-1)]
	}
	return s
}

// statsRanges maps the admin range selector to a window and chart bucket size.
var statsRanges = map[string]struct {
	window    time.Duration
	bucketMin int64
}{
	"1h":  {time.Hour, 1},
	"24h": {24 * time.Hour, 30},
	"7d":  {7 * 24 * time.Hour, 180},
	"30d": {30 * 24 * time.Hour, 720},
}

// StatsReport is the admin statistics payload.
type StatsReport struct {
	Range         string            `json:"range"`
	BucketMinutes int64             `json:"bucket_minutes"`
	From          time.Time         `json:"from"`
	Points        []store.StatPoint `json:"points"`
	Totals        store.StatDelta   `json:"totals"`
	UniqueIPs     int64             `json:"unique_ips"`
	TopPaths      []store.Count     `json:"top_paths"`
	TopIPs        []store.Count     `json:"top_ips"`
	Tunnels       []string          `json:"tunnels"`
}

func (s *Server) statsReport(rng, tunnel string) (StatsReport, error) {
	r, ok := statsRanges[rng]
	if !ok {
		rng, r = "24h", statsRanges["24h"]
	}
	// Include the latest counters that are still in memory.
	if err := s.stats.Flush(); err != nil {
		return StatsReport{}, err
	}
	from := time.Now().Add(-r.window).Truncate(time.Duration(r.bucketMin) * time.Minute)
	fromDay := from.Unix() / 86400
	rep := StatsReport{Range: rng, BucketMinutes: r.bucketMin, From: from.UTC()}

	var err error
	if rep.Points, err = s.store.StatSeries(tunnel, from, r.bucketMin); err != nil {
		return rep, err
	}
	for _, p := range rep.Points {
		t := &rep.Totals
		t.Requests += p.Requests
		t.S2xx += p.S2xx
		t.S3xx += p.S3xx
		t.S4xx += p.S4xx
		t.S5xx += p.S5xx
		t.Conns += p.Conns
		t.BytesIn += p.BytesIn
		t.BytesOut += p.BytesOut
		t.DurMs += p.DurMs
	}
	if rep.UniqueIPs, err = s.store.UniqueIPs(tunnel, fromDay); err != nil {
		return rep, err
	}
	if rep.TopPaths, err = s.store.TopPaths(tunnel, fromDay, 10); err != nil {
		return rep, err
	}
	if rep.TopIPs, err = s.store.TopIPs(tunnel, fromDay, 10); err != nil {
		return rep, err
	}
	rep.Tunnels, err = s.store.StatTunnels(from)
	return rep, err
}
