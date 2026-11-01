package orch

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"apiperf/curlparse"
	"apiperf/metrics"
	"apiperf/store"
)

type collector struct {
	mu sync.Mutex
	ev []Event
}

func (c *collector) emit(e Event) { c.mu.Lock(); c.ev = append(c.ev, e); c.mu.Unlock() }
func (c *collector) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var t []string
	for _, e := range c.ev {
		t = append(t, e.Type)
	}
	return t
}

func setup(t *testing.T, handler http.HandlerFunc) (*store.Store, *httptest.Server, *collector, *Orchestrator) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	c := &collector{}
	o := New(st, c.emit)
	o.SetCooldown(10 * time.Millisecond)
	return st, ts, c, o
}

func waitDone(t *testing.T, c *collector) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, ty := range c.types() {
			if ty == "run_done" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run_done never emitted")
}

func TestHappyRamp(t *testing.T) {
	beforeRun := time.Now()
	st, ts, c, o := setup(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	tgt := curlparse.Target{Method: "GET", URL: ts.URL, Header: http.Header{}}
	plan := []metrics.Stage{{100, 2}, {500, 5}}
	id, _ := st.CreateRun("c", tgt, plan, metrics.DefaultAbortRules())
	if err := o.Start(id, tgt, plan, metrics.DefaultAbortRules()); err != nil {
		t.Fatal(err)
	}
	if err := o.Start(id, tgt, plan, metrics.DefaultAbortRules()); err != ErrBusy {
		t.Fatalf("second start = %v, want ErrBusy", err)
	}
	waitDone(t, c)

	d, err := st.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "completed" || len(d.Summaries) != 2 || d.Verdict == nil {
		t.Fatalf("detail %+v", d)
	}
	if d.Summaries[0].Codes["200"] != 100 || d.Summaries[1].Codes["200"] != 500 {
		t.Fatalf("summaries %+v", d.Summaries)
	}
	afterRun := time.Now()
	beforeMs, afterMs := beforeRun.UnixMilli(), afterRun.UnixMilli()
	for _, s := range d.Summaries {
		if s.StartedAtMs < beforeMs || s.StartedAtMs > afterMs {
			t.Fatalf("stage %d StartedAtMs = %d, want within [%d, %d]", s.Stage, s.StartedAtMs, beforeMs, afterMs)
		}
	}
	if d.Summaries[1].StartedAtMs < d.Summaries[0].StartedAtMs {
		t.Fatalf("stage 1 StartedAtMs (%d) should not precede stage 0's (%d)", d.Summaries[1].StartedAtMs, d.Summaries[0].StartedAtMs)
	}
}

func TestAbortOnErrors(t *testing.T) {
	var hits atomic.Int64
	st, ts, c, o := setup(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack() // hard-close: transport error, not a 5xx
		conn.Close()
	})
	tgt := curlparse.Target{Method: "GET", URL: ts.URL, Header: http.Header{}}
	plan := []metrics.Stage{{50, 5}, {100000, 10}}
	id, _ := st.CreateRun("c", tgt, plan, metrics.DefaultAbortRules())
	o.Start(id, tgt, plan, metrics.DefaultAbortRules())
	waitDone(t, c)

	d, _ := st.GetRun(id)
	if d.Status != "aborted" {
		t.Fatalf("status = %s, want aborted", d.Status)
	}
	if len(d.Summaries) != 1 {
		t.Fatalf("stage 2 should never run, summaries = %d", len(d.Summaries))
	}
	if hits.Load() > 100 { // 50 + retry slack; stage 2's 100k must not have fired
		t.Fatalf("server hits = %d", hits.Load())
	}
	if d.Verdict.AbortReason == "" {
		t.Fatal("abort reason missing")
	}
}

// Test5xxCountsAsFailure verifies that 5xx responses are recorded in Codes,
// do NOT count as transport errors (ErrorCount/ErrorRate), but DO count as
// failures (FailRate) and so trigger the failure-rate abort.
func Test5xxCountsAsFailure(t *testing.T) {
	st, ts, c, o := setup(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	tgt := curlparse.Target{Method: "GET", URL: ts.URL, Header: http.Header{}}
	plan := []metrics.Stage{{5, 2}}
	id, _ := st.CreateRun("c", tgt, plan, metrics.DefaultAbortRules())
	if err := o.Start(id, tgt, plan, metrics.DefaultAbortRules()); err != nil {
		t.Fatal(err)
	}
	waitDone(t, c)

	d, err := st.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "aborted" {
		t.Fatalf("status = %s, want aborted (100%% 503s exceeds the failure-rate rule)", d.Status)
	}
	if len(d.Summaries) != 1 {
		t.Fatalf("summaries = %d", len(d.Summaries))
	}
	sum := d.Summaries[0]
	if sum.Codes["503"] != 5 {
		t.Fatalf("codes[503] = %d, want 5: %+v", sum.Codes["503"], sum.Codes)
	}
	if sum.ErrorCount != 0 {
		t.Fatalf("errorCount = %d, want 0", sum.ErrorCount)
	}
	if sum.ErrorRate != 0 {
		t.Fatalf("errorRate = %v, want 0", sum.ErrorRate)
	}
	if sum.FailCount != 5 || sum.FailRate != 1 {
		t.Fatalf("fail = %d / %v, want 5 / 1", sum.FailCount, sum.FailRate)
	}
}

func TestPerRunTimeout(t *testing.T) {
	st, ts, c, o := setup(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) })
	tgt := curlparse.Target{Method: "GET", URL: ts.URL, Header: http.Header{}}
	plan := []metrics.Stage{{Requests: 2, Concurrency: 2}}
	rules := metrics.AbortRules{MaxErrorRate: 1, MaxP99Ms: 5000, TimeoutMs: 50}
	id, _ := st.CreateRun("c", tgt, plan, rules)
	if err := o.Start(id, tgt, plan, rules); err != nil {
		t.Fatal(err)
	}
	waitDone(t, c)
	d, _ := st.GetRun(id)
	if d.Summaries[0].Timeouts != 2 {
		t.Fatalf("timeouts = %d, want 2 (TimeoutMs=50 vs 300ms handler)", d.Summaries[0].Timeouts)
	}
}

func TestStopWrongRunIDIsNoop(t *testing.T) {
	st, ts, c, o := setup(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
	})
	tgt := curlparse.Target{Method: "GET", URL: ts.URL, Header: http.Header{}}
	plan := []metrics.Stage{{100000, 5}}
	id, _ := st.CreateRun("c", tgt, plan, metrics.DefaultAbortRules())
	o.Start(id, tgt, plan, metrics.DefaultAbortRules())

	time.Sleep(200 * time.Millisecond) // let some requests land
	o.Stop(id + 999)                   // a stale tab targeting a different run id

	if running, activeID := o.Running(); !running || activeID != id {
		t.Fatalf("run should still be active: running=%v activeID=%d", running, activeID)
	}

	o.Stop(id)
	waitDone(t, c)
	d, _ := st.GetRun(id)
	if d.Status != "killed" {
		t.Fatalf("status = %s, want killed", d.Status)
	}
}

func TestKillSwitch(t *testing.T) {
	st, ts, c, o := setup(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
	})
	tgt := curlparse.Target{Method: "GET", URL: ts.URL, Header: http.Header{}}
	plan := []metrics.Stage{{100000, 5}}
	id, _ := st.CreateRun("c", tgt, plan, metrics.DefaultAbortRules())
	o.Start(id, tgt, plan, metrics.DefaultAbortRules())

	time.Sleep(300 * time.Millisecond) // let some requests land
	o.Stop(id)
	waitDone(t, c)

	d, _ := st.GetRun(id)
	if d.Status != "killed" {
		t.Fatalf("status = %s, want killed", d.Status)
	}
	if running, _ := o.Running(); running {
		t.Fatal("orchestrator still marked running")
	}
}
