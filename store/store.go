// Package store persists runs, stage summaries, and per-request results to SQLite.
package store

import (
	"database/sql"
	"encoding/json"
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
}

// ListRuns returns all runs, newest first.
func (s *Store) ListRuns() ([]RunMeta, error) {
	rows, err := s.db.Query(`SELECT id, created_at, curl, status FROM runs ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []RunMeta
	for rows.Next() {
		var m RunMeta
		if err := rows.Scan(&m.ID, &m.CreatedAt, &m.Curl, &m.Status); err != nil {
			return nil, err
		}
		runs = append(runs, m)
	}
	return runs, rows.Err()
}

// GetRun returns the full detail for a run.
func (s *Store) GetRun(id int64) (RunDetail, error) {
	var d RunDetail
	var targetJSON, planJSON, rulesJSON string
	var verdictJSON sql.NullString
	row := s.db.QueryRow(`SELECT id, created_at, curl, status, target_json, plan_json, rules_json, verdict_json FROM runs WHERE id=?`, id)
	if err := row.Scan(&d.ID, &d.CreatedAt, &d.Curl, &d.Status, &targetJSON, &planJSON, &rulesJSON, &verdictJSON); err != nil {
		return RunDetail{}, err
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

// SeriesPoint is one bucket of a time-bucketed latency series.
type SeriesPoint struct {
	OffsetMs int64 `json:"offsetMs"`
	AvgUs    int64 `json:"avgUs"`
	MaxUs    int64 `json:"maxUs"`
	Count    int64 `json:"count"`
}

// Series buckets results by time offset into <=buckets points (SQL GROUP BY).
// span = max(offset_ms)+1; bucket = offset_ms*buckets/span
const seriesQ = `
WITH span AS (SELECT MAX(offset_ms)+1 AS s FROM results WHERE run_id=? AND stage=?)
SELECT MIN(offset_ms), CAST(AVG(latency_us) AS INTEGER), MAX(latency_us), COUNT(*)
FROM results, span WHERE run_id=? AND stage=?
GROUP BY offset_ms * ? / span.s ORDER BY 1`

func (s *Store) Series(runID int64, stage, buckets int) ([]SeriesPoint, error) {
	rows, err := s.db.Query(seriesQ, runID, stage, runID, stage, buckets)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pts []SeriesPoint
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.OffsetMs, &p.AvgUs, &p.MaxUs, &p.Count); err != nil {
			return nil, err
		}
		pts = append(pts, p)
	}
	return pts, rows.Err()
}

// HistBar is one bin of an equal-width latency histogram.
type HistBar struct {
	FromUs int64 `json:"fromUs"`
	Count  int64 `json:"count"`
}

// Histogram groups latencies into ~bins equal-width buckets.
const histQ = `
WITH b AS (SELECT MIN(latency_us) AS lo, (MAX(latency_us)-MIN(latency_us))/? + 1 AS w
           FROM results WHERE run_id=? AND stage=?)
SELECT b.lo + ((latency_us - b.lo)/b.w)*b.w AS from_us, COUNT(*)
FROM results, b WHERE run_id=? AND stage=?
GROUP BY (latency_us - b.lo)/b.w ORDER BY 1`

func (s *Store) Histogram(runID int64, stage, bins int) ([]HistBar, error) {
	rows, err := s.db.Query(histQ, bins, runID, stage, runID, stage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bars []HistBar
	for rows.Next() {
		var b HistBar
		if err := rows.Scan(&b.FromUs, &b.Count); err != nil {
			return nil, err
		}
		bars = append(bars, b)
	}
	return bars, rows.Err()
}
