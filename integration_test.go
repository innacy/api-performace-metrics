package main_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"apiperf/orch"
	"apiperf/server"
	"apiperf/store"
)

// Full loop: create → start → poll to completion → verify exact counts,
// summaries, verdict, series — through the real HTTP surface.
func TestFullRamp(t *testing.T) {
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer target.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hub := server.NewHub()
	o := orch.New(st, hub.Emit)
	o.SetCooldown(10 * time.Millisecond)
	app := httptest.NewServer(server.New(st, o, hub))
	defer app.Close()

	payload := fmt.Sprintf(`{"curl":"curl %s","stages":[{"requests":100,"concurrency":2},{"requests":500,"concurrency":5}]}`, target.URL)
	res, _ := http.Post(app.URL+"/api/runs", "application/json", strings.NewReader(payload))
	var created struct {
		RunID int64 `json:"runId"`
	}
	json.NewDecoder(res.Body).Decode(&created)

	res, _ = http.Post(fmt.Sprintf("%s/api/runs/%d/start", app.URL, created.RunID), "application/json", strings.NewReader(`{}`))
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("start: %d %s", res.StatusCode, b)
	}

	var detail store.RunDetail
	deadline := time.Now().Add(30 * time.Second)
	for {
		detail, err = st.GetRun(created.RunID)
		if err == nil && detail.Status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %s, never completed", detail.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if hits.Load() != 602 { // 600 load + 2 probe (cold + warm)
		t.Errorf("target hits = %d, want 602", hits.Load())
	}
	if len(detail.Summaries) != 2 {
		t.Fatalf("summaries = %d", len(detail.Summaries))
	}
	s0, s1 := detail.Summaries[0], detail.Summaries[1]
	if s0.Codes["200"] != 100 || s1.Codes["200"] != 500 {
		t.Errorf("codes: %+v / %+v", s0.Codes, s1.Codes)
	}
	if s0.P50Us <= 0 || s0.P99Us < s0.P50Us {
		t.Errorf("percentile sanity: %+v", s0)
	}
	if detail.Verdict == nil {
		t.Fatal("verdict missing")
	}

	var pts []store.SeriesPoint
	res, _ = http.Get(fmt.Sprintf("%s/api/runs/%d/series?stage=1", app.URL, created.RunID))
	json.NewDecoder(res.Body).Decode(&pts)
	var total int64
	for _, p := range pts {
		total += p.Count
	}
	if total != 500 {
		t.Errorf("stage 1 series total = %d, want 500", total)
	}
}
