package store

import (
	"path/filepath"
	"testing"

	"apiperf/curlparse"
	"apiperf/metrics"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRunLifecycle(t *testing.T) {
	s := open(t)
	tgt := curlparse.Target{Method: "GET", URL: "http://h/x"}
	id, err := s.CreateRun("curl http://h/x", tgt, metrics.DefaultPlan(), metrics.DefaultAbortRules())
	if err != nil {
		t.Fatal(err)
	}

	// 2500 samples in one call exercises batching
	batch := make([]metrics.Sample, 2500)
	for i := range batch {
		batch[i] = metrics.Sample{OffsetMs: int64(i), LatencyUs: int64(100 + i%50), Code: 200}
	}
	if err := s.InsertResults(id, 0, batch); err != nil {
		t.Fatal(err)
	}

	sum := metrics.StageSummary{Stage: 0, Requests: 2500, P99Us: 149}
	if err := s.SaveStageSummary(id, sum); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(id, "completed", metrics.ComputeVerdict([]metrics.StageSummary{sum}, "")); err != nil {
		t.Fatal(err)
	}

	runs, err := s.ListRuns()
	if err != nil || len(runs) != 1 || runs[0].Status != "completed" {
		t.Fatalf("ListRuns = %+v, %v", runs, err)
	}
	d, err := s.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Summaries) != 1 || d.Summaries[0].P99Us != 149 || d.Verdict == nil {
		t.Fatalf("detail %+v", d)
	}
	if len(d.Plan) != 5 || d.Target.URL != "http://h/x" {
		t.Fatalf("plan/target %+v", d)
	}
}

func TestProbeAndListHeadlines(t *testing.T) {
	s := open(t)
	id, _ := s.CreateRun("c", curlparse.Target{}, metrics.DefaultPlan(), metrics.DefaultAbortRules())
	if err := s.SetProbe(id, map[string]any{"status": 200}); err != nil {
		t.Fatal(err)
	}
	s.SaveStageSummary(id, metrics.StageSummary{Stage: 0, RPS: 100, P99Us: 10})
	s.SaveStageSummary(id, metrics.StageSummary{Stage: 1, RPS: 300, P99Us: 99})
	s.FinishRun(id, "completed", metrics.Verdict{MaxHealthyRPS: 300})

	d, err := s.GetRun(id)
	if err != nil || string(d.Probe) != `{"status":200}` {
		t.Fatalf("probe = %s, %v", d.Probe, err)
	}
	runs, err := s.ListRuns()
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns = %+v, %v", runs, err)
	}
	r := runs[0]
	if r.Verdict == nil || r.Verdict.MaxHealthyRPS != 300 || r.Peak == nil || r.Peak.P99Us != 99 || r.Stages != 2 {
		t.Fatalf("headlines = %+v", r)
	}
}

func TestOpenMigratesOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, _ := Open(path)
	s.db.Exec(`ALTER TABLE runs DROP COLUMN probe_json`)
	s.Close()
	s, err := Open(path) // must re-add the column
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, _ := s.CreateRun("c", curlparse.Target{}, nil, metrics.AbortRules{})
	if err := s.SetProbe(id, 1); err != nil {
		t.Fatal(err)
	}
}

func TestMarkStaleRunsKilled(t *testing.T) {
	s := open(t)
	tgt := curlparse.Target{Method: "GET", URL: "http://h/x"}

	runningID, err := s.CreateRun("curl http://h/x", tgt, metrics.DefaultPlan(), metrics.DefaultAbortRules())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(runningID, "running"); err != nil {
		t.Fatal(err)
	}

	pendingID, err := s.CreateRun("curl http://h/y", tgt, metrics.DefaultPlan(), metrics.DefaultAbortRules())
	if err != nil {
		t.Fatal(err)
	}

	completedID, err := s.CreateRun("curl http://h/z", tgt, metrics.DefaultPlan(), metrics.DefaultAbortRules())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(completedID, "completed", metrics.Verdict{}); err != nil {
		t.Fatal(err)
	}

	n, err := s.MarkStaleRunsKilled()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}

	d, err := s.GetRun(runningID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "killed" {
		t.Fatalf("running run status = %s, want killed", d.Status)
	}

	d, err = s.GetRun(pendingID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "pending" {
		t.Fatalf("pending run status = %s, want unchanged (pending)", d.Status)
	}

	d, err = s.GetRun(completedID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "completed" {
		t.Fatalf("completed run status = %s, want unchanged (completed)", d.Status)
	}
}

func TestSeriesAndHistogram(t *testing.T) {
	s := open(t)
	id, _ := s.CreateRun("c", curlparse.Target{}, nil, metrics.AbortRules{})
	batch := make([]metrics.Sample, 10000)
	for i := range batch {
		code := 200
		if i%10 == 0 {
			code = 503
		}
		batch[i] = metrics.Sample{OffsetMs: int64(i), LatencyUs: int64(1000 + i%100), Code: code}
	}
	s.InsertResults(id, 0, batch)

	pts, err := s.Series(id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) == 0 || len(pts) > 101 {
		t.Fatalf("series len = %d", len(pts))
	}
	var total, c5xx int64
	for _, p := range pts {
		total += p.Count
		c5xx += p.C5xx
		if p.P50Us < 1000 || p.P50Us > p.P95Us || p.P95Us > p.P99Us || p.P99Us > 1099 {
			t.Fatalf("bad percentiles %+v", p)
		}
	}
	if total != 10000 || c5xx != 1000 {
		t.Fatalf("series counts sum = %d, 5xx = %d", total, c5xx)
	}

	bars, err := s.Histogram(id, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	total = 0
	for _, b := range bars {
		total += b.Count
	}
	if total != 10000 || len(bars) != 50 || bars[0].FromUs != 1000 || bars[49].ToUs != 1100 {
		t.Fatalf("hist sum = %d, bars = %d, range %d..%d", total, len(bars), bars[0].FromUs, bars[len(bars)-1].ToUs)
	}
}
