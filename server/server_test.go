package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"apiperf/engine"
	"apiperf/orch"
	"apiperf/store"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hub := NewHub()
	o := orch.New(st, hub.Emit)
	o.SetCooldown(10 * time.Millisecond)
	ts := httptest.NewServer(New(st, o, hub))
	t.Cleanup(ts.Close)
	return ts, st
}

func TestServesUIAndPing(t *testing.T) {
	ts, _ := newTestServer(t)

	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("GET / status = %d", res.StatusCode)
	}

	res, err = http.Get(ts.URL + "/api/ping")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _ := res.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), `"ok"`) {
		t.Fatalf("ping body = %s", buf[:n])
	}
}

func TestCreateRunParseError(t *testing.T) {
	ts, _ := newTestServer(t)
	res, _ := http.Post(ts.URL+"/api/runs", "application/json",
		strings.NewReader(`{"curl":"curl --compressed http://h/x"}`))
	if res.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "--compressed") {
		t.Fatalf("error should name the flag: %s", body)
	}
}

func TestCreateRunRejectsInvalidStages(t *testing.T) {
	ts, _ := newTestServer(t)

	cases := []struct {
		name    string
		stages  string
		wantErr string
	}{
		{"zero concurrency", `[{"requests":10,"concurrency":0}]`, "concurrency"},
		{"zero requests", `[{"requests":0,"concurrency":5}]`, "requests"},
		{"negative concurrency", `[{"requests":10,"concurrency":-1}]`, "concurrency"},
		{"empty stages", `[]`, "stages"},
		{"concurrency over cap", `[{"requests":10,"concurrency":10001}]`, "concurrency"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"curl":"curl http://h/x","stages":%s}`, c.stages)
			res, err := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != 400 {
				t.Fatalf("status = %d, want 400", res.StatusCode)
			}
			body, _ := io.ReadAll(res.Body)
			if !strings.Contains(string(body), c.wantErr) {
				t.Fatalf("error should mention %q: %s", c.wantErr, body)
			}
		})
	}
}

func TestCreateAndRunEndToEnd(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer target.Close()
	ts, st := newTestServer(t)

	payload := fmt.Sprintf(`{"curl":"curl %s","stages":[{"requests":50,"concurrency":5}]}`, target.URL)
	res, _ := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(payload))
	if res.StatusCode != 201 {
		t.Fatalf("create status = %d", res.StatusCode)
	}
	var created struct {
		RunID    int64              `json:"runId"`
		Probe    engine.ProbeResult `json:"probe"`
		External bool               `json:"external"`
	}
	json.NewDecoder(res.Body).Decode(&created)
	if created.Probe.Status != 200 || created.External {
		t.Fatalf("created = %+v", created)
	}

	res, _ = http.Post(fmt.Sprintf("%s/api/runs/%d/start", ts.URL, created.RunID), "application/json", strings.NewReader(`{}`))
	if res.StatusCode != 200 {
		t.Fatalf("start status = %d", res.StatusCode)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		d, err := st.GetRun(created.RunID)
		if err == nil && (d.Status == "completed" || d.Status == "aborted") {
			if d.Status != "completed" {
				t.Fatalf("final status = %s", d.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run never completed")
		}
		time.Sleep(50 * time.Millisecond)
	}

	res, _ = http.Get(fmt.Sprintf("%s/api/runs/%d/series?stage=0", ts.URL, created.RunID))
	var pts []store.SeriesPoint
	json.NewDecoder(res.Body).Decode(&pts)
	var total int64
	for _, p := range pts {
		total += p.Count
	}
	if total != 50 {
		t.Fatalf("series total = %d", total)
	}
}

func TestSSEDeliversEvents(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer target.Close()
	ts, _ := newTestServer(t)

	sseRes, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer sseRes.Body.Close()

	payload := fmt.Sprintf(`{"curl":"curl %s","stages":[{"requests":20,"concurrency":2}]}`, target.URL)
	res, _ := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(payload))
	var created struct {
		RunID int64 `json:"runId"`
	}
	json.NewDecoder(res.Body).Decode(&created)
	http.Post(fmt.Sprintf("%s/api/runs/%d/start", ts.URL, created.RunID), "application/json", strings.NewReader(`{}`))

	scanner := bufio.NewScanner(sseRes.Body)
	sawStageDone, sawRunDone := false, false
	timeout := time.AfterFunc(20*time.Second, func() { sseRes.Body.Close() })
	defer timeout.Stop()
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, `"stage_done"`) {
			sawStageDone = true
		}
		if strings.Contains(line, `"run_done"`) {
			sawRunDone = true
			break
		}
	}
	if !sawStageDone || !sawRunDone {
		t.Fatalf("stage_done=%v run_done=%v", sawStageDone, sawRunDone)
	}
}

func TestStartRunTwiceRejected(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer target.Close()
	ts, st := newTestServer(t)

	payload := fmt.Sprintf(`{"curl":"curl %s","stages":[{"requests":30,"concurrency":5}]}`, target.URL)
	res, _ := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(payload))
	if res.StatusCode != 201 {
		t.Fatalf("create status = %d", res.StatusCode)
	}
	var created struct {
		RunID int64 `json:"runId"`
	}
	json.NewDecoder(res.Body).Decode(&created)

	res, _ = http.Post(fmt.Sprintf("%s/api/runs/%d/start", ts.URL, created.RunID), "application/json", strings.NewReader(`{}`))
	if res.StatusCode != 200 {
		t.Fatalf("first start status = %d", res.StatusCode)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		d, err := st.GetRun(created.RunID)
		if err == nil && (d.Status == "completed" || d.Status == "aborted") {
			if d.Status != "completed" {
				t.Fatalf("final status = %s", d.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run never completed")
		}
		time.Sleep(50 * time.Millisecond)
	}

	res, _ = http.Post(fmt.Sprintf("%s/api/runs/%d/start", ts.URL, created.RunID), "application/json", strings.NewReader(`{}`))
	if res.StatusCode != 409 {
		t.Fatalf("second start status = %d, want 409", res.StatusCode)
	}

	res, _ = http.Get(fmt.Sprintf("%s/api/runs/%d/series?stage=0", ts.URL, created.RunID))
	var pts []store.SeriesPoint
	json.NewDecoder(res.Body).Decode(&pts)
	var total int64
	for _, p := range pts {
		total += p.Count
	}
	if total != 30 {
		t.Fatalf("series total after rejected re-start = %d, want 30", total)
	}
}
