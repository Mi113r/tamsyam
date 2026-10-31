package store

import (
	"database/sql"
	"errors"
	"time"
)

const statsSchema = `
CREATE TABLE IF NOT EXISTS stats (
	tunnel    TEXT    NOT NULL,
	minute    INTEGER NOT NULL,
	requests  INTEGER NOT NULL DEFAULT 0,
	s2xx      INTEGER NOT NULL DEFAULT 0,
	s3xx      INTEGER NOT NULL DEFAULT 0,
	s4xx      INTEGER NOT NULL DEFAULT 0,
	s5xx      INTEGER NOT NULL DEFAULT 0,
	conns     INTEGER NOT NULL DEFAULT 0,
	bytes_in  INTEGER NOT NULL DEFAULT 0,
	bytes_out INTEGER NOT NULL DEFAULT 0,
	dur_ms    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (tunnel, minute)
);
CREATE TABLE IF NOT EXISTS stats_ips (
	tunnel TEXT    NOT NULL,
	day    INTEGER NOT NULL,
	ip     TEXT    NOT NULL,
	hits   INTEGER NOT NULL,
	PRIMARY KEY (tunnel, day, ip)
);
CREATE TABLE IF NOT EXISTS stats_paths (
	tunnel TEXT    NOT NULL,
	day    INTEGER NOT NULL,
	path   TEXT    NOT NULL,
	hits   INTEGER NOT NULL,
	PRIMARY KEY (tunnel, day, path)
);
CREATE TABLE IF NOT EXISTS port_reservations (
	port       INTEGER PRIMARY KEY,
	token_id   INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);`

// StatDelta is a set of counters for one tunnel and minute (or an aggregate of them).
type StatDelta struct {
	Requests int64 `json:"requests"`
	S2xx     int64 `json:"s2xx"`
	S3xx     int64 `json:"s3xx"`
	S4xx     int64 `json:"s4xx"`
	S5xx     int64 `json:"s5xx"`
	Conns    int64 `json:"conns"`
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`
	DurMs    int64 `json:"dur_ms"` // sum over requests; divide by Requests for the mean
}

type StatKey struct {
	Tunnel string
	Minute int64 // unix time / 60
}

// DayKey identifies a per-day counter of a value (client IP or path).
type DayKey struct {
	Tunnel string
	Day    int64 // unix time / 86400
	Value  string
}

type StatPoint struct {
	Time time.Time `json:"t"`
	StatDelta
}

type Count struct {
	Value string `json:"value"`
	Hits  int64  `json:"hits"`
}

// AddStats adds the counters to the stored totals in one transaction.
func (s *Store) AddStats(deltas map[StatKey]*StatDelta, ips, paths map[DayKey]int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	for k, d := range deltas {
		if _, err := tx.Exec(`
			INSERT INTO stats (tunnel, minute, requests, s2xx, s3xx, s4xx, s5xx, conns, bytes_in, bytes_out, dur_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (tunnel, minute) DO UPDATE SET
				requests = requests + excluded.requests,
				s2xx = s2xx + excluded.s2xx, s3xx = s3xx + excluded.s3xx,
				s4xx = s4xx + excluded.s4xx, s5xx = s5xx + excluded.s5xx,
				conns = conns + excluded.conns,
				bytes_in = bytes_in + excluded.bytes_in, bytes_out = bytes_out + excluded.bytes_out,
				dur_ms = dur_ms + excluded.dur_ms`,
			k.Tunnel, k.Minute, d.Requests, d.S2xx, d.S3xx, d.S4xx, d.S5xx, d.Conns, d.BytesIn, d.BytesOut, d.DurMs); err != nil {
			return err
		}
	}
	for table, counts := range map[string]map[DayKey]int64{"stats_ips": ips, "stats_paths": paths} {
		col := "ip"
		if table == "stats_paths" {
			col = "path"
		}
		for k, n := range counts {
			if _, err := tx.Exec(`INSERT INTO `+table+` (tunnel, day, `+col+`, hits) VALUES (?, ?, ?, ?)
				ON CONFLICT DO UPDATE SET hits = hits + excluded.hits`, k.Tunnel, k.Day, k.Value, n); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// StatSeries returns counters since from, grouped into buckets of bucketMin
// minutes. An empty tunnel aggregates all tunnels.
func (s *Store) StatSeries(tunnel string, from time.Time, bucketMin int64) ([]StatPoint, error) {
	rows, err := s.db.Query(`
		SELECT (minute / ?1) * ?1 AS b, SUM(requests), SUM(s2xx), SUM(s3xx), SUM(s4xx), SUM(s5xx),
		       SUM(conns), SUM(bytes_in), SUM(bytes_out), SUM(dur_ms)
		FROM stats WHERE minute >= ?2 AND (?3 = '' OR tunnel = ?3)
		GROUP BY b ORDER BY b`, bucketMin, from.Unix()/60, tunnel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []StatPoint{}
	for rows.Next() {
		var (
			p StatPoint
			b int64
		)
		if err := rows.Scan(&b, &p.Requests, &p.S2xx, &p.S3xx, &p.S4xx, &p.S5xx,
			&p.Conns, &p.BytesIn, &p.BytesOut, &p.DurMs); err != nil {
			return nil, err
		}
		p.Time = time.Unix(b*60, 0).UTC()
		points = append(points, p)
	}
	return points, rows.Err()
}

// TopIPs and TopPaths return the most frequent values since fromDay.
func (s *Store) TopIPs(tunnel string, fromDay int64, limit int) ([]Count, error) {
	return s.topCounts("stats_ips", "ip", tunnel, fromDay, limit)
}

func (s *Store) TopPaths(tunnel string, fromDay int64, limit int) ([]Count, error) {
	return s.topCounts("stats_paths", "path", tunnel, fromDay, limit)
}

func (s *Store) topCounts(table, col, tunnel string, fromDay int64, limit int) ([]Count, error) {
	rows, err := s.db.Query(`SELECT `+col+`, SUM(hits) AS n FROM `+table+`
		WHERE day >= ?1 AND (?2 = '' OR tunnel = ?2) GROUP BY `+col+` ORDER BY n DESC LIMIT ?3`,
		fromDay, tunnel, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Count{}
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Value, &c.Hits); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) UniqueIPs(tunnel string, fromDay int64) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(DISTINCT ip) FROM stats_ips WHERE day >= ?1 AND (?2 = '' OR tunnel = ?2)`,
		fromDay, tunnel).Scan(&n)
	return n, err
}

// StatTunnels lists tunnels that have stats since from.
func (s *Store) StatTunnels(from time.Time) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT tunnel FROM stats WHERE minute >= ? ORDER BY tunnel`, from.Unix()/60)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PurgeStats deletes counters older than before.
func (s *Store) PurgeStats(before time.Time) error {
	if _, err := s.db.Exec(`DELETE FROM stats WHERE minute < ?`, before.Unix()/60); err != nil {
		return err
	}
	day := before.Unix() / 86400
	if _, err := s.db.Exec(`DELETE FROM stats_ips WHERE day < ?`, day); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM stats_paths WHERE day < ?`, day)
	return err
}

type Reservation struct {
	Port      int       `json:"port"`
	TokenID   int64     `json:"token_id"`
	TokenName string    `json:"token_name"`
	CreatedAt time.Time `json:"created_at"`
}

// ReservePort pins port to tokenID unless another token already holds it,
// and returns the owner.
func (s *Store) ReservePort(port int, tokenID int64) (int64, error) {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO port_reservations (port, token_id, created_at) VALUES (?, ?, ?)`,
		port, tokenID, time.Now().UTC().Unix()); err != nil {
		return 0, err
	}
	owner, _, err := s.PortOwner(port)
	return owner, err
}

func (s *Store) PortOwner(port int) (int64, bool, error) {
	var owner int64
	err := s.db.QueryRow(`SELECT token_id FROM port_reservations WHERE port = ?`, port).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return owner, err == nil, err
}

func (s *Store) ListReservations() ([]Reservation, error) {
	rows, err := s.db.Query(`SELECT r.port, r.token_id, COALESCE(t.name, ''), r.created_at
		FROM port_reservations r LEFT JOIN tokens t ON t.id = r.token_id ORDER BY r.port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reservation{}
	for rows.Next() {
		var (
			r       Reservation
			created int64
		)
		if err := rows.Scan(&r.Port, &r.TokenID, &r.TokenName, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ReleasePort(port int) error {
	res, err := s.db.Exec(`DELETE FROM port_reservations WHERE port = ?`, port)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
