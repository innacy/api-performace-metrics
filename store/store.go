// Package store persists runs, stage summaries, and per-request results to SQLite.
package store

import (
	"database/sql"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"

	"apiperf/curlparse"
	"apiperf/metrics"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS runs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at TEXT NOT NULL,
  curl TEXT NOT NULL,
  target_json TEXT NOT NULL,
  plan_json TEXT NOT NULL,
  rules_json TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  verdict_json TEXT
);
CREATE TABLE IF NOT EXISTS stage_summaries(
  run_id INTEGER NOT NULL, stage INTEGER NOT NULL, summary_json TEXT NOT NULL,
  PRIMARY KEY(run_id, stage)
);
CREATE TABLE IF NOT EXISTS results(
  run_id INTEGER NOT NULL, stage INTEGER NOT NULL,
  offset_ms INTEGER NOT NULL, latency_us INTEGER NOT NULL,
  code INTEGER NOT NULL, error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_results ON results(run_id, stage);
`

// Store wraps a SQLite database holding runs, stage summaries, and results.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Added after v1: older databases lack the column.
	if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN probe_json TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// RunMeta is the summary row for a run.
type RunMeta struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"createdAt"` // RFC3339
	Curl      string `json:"curl"`
	Status    string `json:"status"` // pending|running|completed|aborted|killed
}

// RunListItem is a History row: run meta plus headline results.
type RunListItem struct {
	RunMeta
	Verdict *metrics.Verdict      `json:"verdict"`
	Peak    *metrics.StageSummary `json:"peak"` // last completed stage
	Stages  int                   `json:"stages"`
}

// SetProbe stores the Verify probe result (any JSON-encodable value) for a run.
func (s *Store) SetProbe(runID int64, probe any) error {
	b, err := json.Marshal(probe)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE runs SET probe_json=? WHERE id=?`, string(b), runID)
	return err
}

// CreateRun inserts a new run with status=pending and returns its ID.
func (s *Store) CreateRun(curl string, tgt curlparse.Target, plan []metrics.Stage, rules metrics.AbortRules) (int64, error) {
	targetJSON, err := json.Marshal(tgt)
	if err != nil {
		return 0, err
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return 0, err
	}
	rulesJSON, err := json.Marshal(rules)
	if err != nil {
		return 0, err
	}
	createdAt := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		`INSERT INTO runs(created_at, curl, target_json, plan_json, rules_json, status) VALUES(?,?,?,?,?,'pending')`,
		createdAt, curl, string(targetJSON), string(planJSON), string(rulesJSON),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetStatus updates a run's status.
func (s *Store) SetStatus(runID int64, status string) error {
	_, err := s.db.Exec(`UPDATE runs SET status=? WHERE id=?`, status, runID)
	return err
}

// MarkStaleRunsKilled marks any run left in status='running' as 'killed'.
// Call this on startup: a run stuck at 'running' means the previous process
// was killed (e.g. Ctrl-C without graceful shutdown, a crash) mid-run, so
// it can never actually finish. Returns the number of rows updated.
func (s *Store) MarkStaleRunsKilled() (int, error) {
	res, err := s.db.Exec(`UPDATE runs SET status='killed' WHERE status='running'`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// InsertResults inserts a batch of samples for a stage in a single transaction with a prepared statement.
func (s *Store) InsertResults(runID int64, stage int, batch []metrics.Sample) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO results(run_id,stage,offset_ms,latency_us,code,error) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, r := range batch {
		if _, err := stmt.Exec(runID, stage, r.OffsetMs, r.LatencyUs, r.Code, r.Error); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// SaveStageSummary stores a stage's summary as JSON.
func (s *Store) SaveStageSummary(runID int64, sum metrics.StageSummary) error {
	sumJSON, err := json.Marshal(sum)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO stage_summaries(run_id, stage, summary_json) VALUES(?,?,?)`,
		runID, sum.Stage, string(sumJSON),
	)
	return err
}

// FinishRun sets the final status and verdict for a run.
func (s *Store) FinishRun(runID int64, status string, v metrics.Verdict) error {
	verdictJSON, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE runs SET status=?, verdict_json=? WHERE id=?`, status, string(verdictJSON), runID)
	return err
}

// RunDetail is the full detail view of a run.
type RunDetail struct {
	RunMeta
	Plan      []metrics.Stage        `json:"plan"`
	Rules     metrics.AbortRules     `json:"rules"`
	Summaries []metrics.StageSummary `json:"summaries"`
	Verdict   *metrics.Verdict       `json:"verdict"` // nil while running
	Target    curlparse.Target       `json:"target"`
	Probe     json.RawMessage        `json:"probe,omitempty"` // engine.ProbeResult from Verify
}

// ListRuns returns all runs, newest first, with headline results.
func (s *Store) ListRuns() ([]RunListItem, error) {
	rows, err := s.db.Query(`
SELECT r.id, r.created_at, r.curl, r.status, r.verdict_json,
  (SELECT summary_json FROM stage_summaries WHERE run_id=r.id ORDER BY stage DESC LIMIT 1),
  (SELECT COUNT(*) FROM stage_summaries WHERE run_id=r.id)
FROM runs r ORDER BY r.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []RunListItem
	for rows.Next() {
		var m RunListItem
		var verdictJSON, peakJSON sql.NullString
		if err := rows.Scan(&m.ID, &m.CreatedAt, &m.Curl, &m.Status, &verdictJSON, &peakJSON, &m.Stages); err != nil {
			return nil, err
		}
		if verdictJSON.Valid {
			m.Verdict = new(metrics.Verdict)
			if err := json.Unmarshal([]byte(verdictJSON.String), m.Verdict); err != nil {
				return nil, err
			}
		}
		if peakJSON.Valid {
			m.Peak = new(metrics.StageSummary)
			if err := json.Unmarshal([]byte(peakJSON.String), m.Peak); err != nil {
				return nil, err
			}
		}
		runs = append(runs, m)
	}
	return runs, rows.Err()
}

// GetRun returns the full detail for a run.
func (s *Store) GetRun(id int64) (RunDetail, error) {
	var d RunDetail
	var targetJSON, planJSON, rulesJSON string
	var verdictJSON, probeJSON sql.NullString
	row := s.db.QueryRow(`SELECT id, created_at, curl, status, target_json, plan_json, rules_json, verdict_json, probe_json FROM runs WHERE id=?`, id)
	if err := row.Scan(&d.ID, &d.CreatedAt, &d.Curl, &d.Status, &targetJSON, &planJSON, &rulesJSON, &verdictJSON, &probeJSON); err != nil {
		return RunDetail{}, err
	}
	if probeJSON.Valid {
		d.Probe = json.RawMessage(probeJSON.String)
	}
	if err := json.Unmarshal([]byte(targetJSON), &d.Target); err != nil {
		return RunDetail{}, err
	}
	if err := json.Unmarshal([]byte(planJSON), &d.Plan); err != nil {
		return RunDetail{}, err
	}
	if err := json.Unmarshal([]byte(rulesJSON), &d.Rules); err != nil {
		return RunDetail{}, err
	}
	if verdictJSON.Valid {
		var v metrics.Verdict
		if err := json.Unmarshal([]byte(verdictJSON.String), &v); err != nil {
			return RunDetail{}, err
		}
		d.Verdict = &v
	}

	rows, err := s.db.Query(`SELECT summary_json FROM stage_summaries WHERE run_id=? ORDER BY stage`, id)
	if err != nil {
		return RunDetail{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var sumJSON string
		if err := rows.Scan(&sumJSON); err != nil {
			return RunDetail{}, err
		}
		var sum metrics.StageSummary
		if err := json.Unmarshal([]byte(sumJSON), &sum); err != nil {
			return RunDetail{}, err
		}
		d.Summaries = append(d.Summaries, sum)
	}
	if err := rows.Err(); err != nil {
		return RunDetail{}, err
	}
	return d, nil
}

// SeriesPoint is one time bucket of a stage: latency percentiles and response mix.
type SeriesPoint struct {
	OffsetMs int64 `json:"offsetMs"` // bucket start, ms since stage start
	Count    int64 `json:"count"`
	P50Us    int64 `json:"p50Us"`
	P95Us    int64 `json:"p95Us"`
	P99Us    int64 `json:"p99Us"`
	OK       int64 `json:"ok"` // < 400
	C4xx     int64 `json:"c4xx"`
	C5xx     int64 `json:"c5xx"`
	Errors   int64 `json:"errors"` // transport errors/timeouts (code 0)
}

// stageRows loads one stage's raw results.
// ponytail: holds a whole stage in memory (~24 B/row, ~24 MB at 1M); fine for one report view.
func (s *Store) stageRows(runID int64, stage int) (offs, lats []int64, codes []int, err error) {
	rows, err := s.db.Query(`SELECT offset_ms, latency_us, code FROM results WHERE run_id=? AND stage=?`, runID, stage)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var o, l int64
		var c int
		if err := rows.Scan(&o, &l, &c); err != nil {
			return nil, nil, nil, err
		}
		offs, lats, codes = append(offs, o), append(lats, l), append(codes, c)
	}
	return offs, lats, codes, rows.Err()
}

// Series splits a stage into <=buckets equal time buckets (empty ones omitted).
func (s *Store) Series(runID int64, stage, buckets int) ([]SeriesPoint, error) {
	offs, lats, codes, err := s.stageRows(runID, stage)
	if err != nil || len(offs) == 0 {
		return nil, err
	}
	span := slices.Max(offs) + 1
	pts := make([]SeriesPoint, buckets)
	byBucket := make([][]int64, buckets)
	for i, o := range offs {
		b := int(o * int64(buckets) / span)
		p := &pts[b]
		p.Count++
		switch c := codes[i]; {
		case c == 0:
			p.Errors++
		case c >= 500:
			p.C5xx++
		case c >= 400:
			p.C4xx++
		default:
			p.OK++
		}
		byBucket[b] = append(byBucket[b], lats[i])
	}
	var out []SeriesPoint
	for b, p := range pts {
		if p.Count == 0 {
			continue
		}
		l := byBucket[b]
		slices.Sort(l)
		p.OffsetMs = int64(b) * span / int64(buckets)
		p.P50Us, p.P95Us, p.P99Us = metrics.Percentile(l, .50), metrics.Percentile(l, .95), metrics.Percentile(l, .99)
		out = append(out, p)
	}
	return out, nil
}

// HistBar is one bin of a log-scale latency histogram: [FromUs, ToUs).
type HistBar struct {
	FromUs int64 `json:"fromUs"`
	ToUs   int64 `json:"toUs"`
	Count  int64 `json:"count"`
}

// Histogram groups a stage's latencies into `bins` log-spaced bins between
// min and max (all bins returned, including empty ones, so the axis is continuous).
func (s *Store) Histogram(runID int64, stage, bins int) ([]HistBar, error) {
	_, lats, _, err := s.stageRows(runID, stage)
	if err != nil || len(lats) == 0 {
		return nil, err
	}
	lo, hi := float64(max(slices.Min(lats), 1)), float64(max(slices.Max(lats), 1))
	span := math.Log(hi / lo)
	if span == 0 {
		return []HistBar{{FromUs: int64(lo), ToUs: int64(hi) + 1, Count: int64(len(lats))}}, nil
	}
	bars := make([]HistBar, bins)
	for i := range bars {
		bars[i].FromUs = int64(lo * math.Exp(span*float64(i)/float64(bins)))
		bars[i].ToUs = int64(lo * math.Exp(span*float64(i+1)/float64(bins)))
	}
	bars[bins-1].ToUs = int64(hi) + 1
	for _, v := range lats {
		i := int(math.Log(float64(max(v, 1))/lo) / span * float64(bins))
		bars[min(max(i, 0), bins-1)].Count++
	}
	return bars, nil
}
