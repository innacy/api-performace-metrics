package metrics

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	s := make([]int64, 100)
	for i := range s {
		s[i] = int64(i + 1) // 1..100
	}
	for _, c := range []struct {
		q    float64
		want int64
	}{{0.50, 50}, {0.90, 90}, {0.95, 95}, {0.99, 99}, {1.0, 100}} {
		if got := Percentile(s, c.q); got != c.want {
			t.Errorf("p%.0f = %d, want %d", c.q*100, got, c.want)
		}
	}
	if Percentile([]int64{42}, 0.99) != 42 {
		t.Error("single element")
	}
	if Percentile(nil, 0.5) != 0 {
		t.Error("empty -> 0")
	}
}

func TestStageAccSummarize(t *testing.T) {
	a := NewStageAcc()
	for i := 0; i < 90; i++ {
		a.Add(Sample{LatencyUs: int64(1000 + i), Code: 200})
	}
	for i := 0; i < 8; i++ {
		a.Add(Sample{LatencyUs: 5000, Code: 500})
	}
	a.Add(Sample{LatencyUs: 10_000_000, Error: "timeout", Timeout: true})
	a.Add(Sample{LatencyUs: 9000, Error: "connection refused"})

	startedAt := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	s := a.Summarize(2, Stage{Requests: 100, Concurrency: 10}, 2*time.Second, startedAt)
	if s.Stage != 2 || s.Codes["200"] != 90 || s.Codes["500"] != 8 {
		t.Fatalf("summary %+v", s)
	}
	if s.StartedAtMs != startedAt.UnixMilli() {
		t.Fatalf("StartedAtMs = %d, want %d", s.StartedAtMs, startedAt.UnixMilli())
	}
	if s.ErrorCount != 2 || s.Timeouts != 1 {
		t.Fatalf("errors %+v", s)
	}
	if s.FailCount != 10 || s.FailRate != 0.1 { // 8×500 + 2 transport errors
		t.Fatalf("fail = %d / %v", s.FailCount, s.FailRate)
	}
	if s.ErrorRate != 0.02 {
		t.Fatalf("errorRate = %v", s.ErrorRate)
	}
	if s.RPS != 50 { // 100 requests / 2s
		t.Fatalf("rps = %v", s.RPS)
	}
	if s.MinUs != 1000 || s.MaxUs != 10_000_000 {
		t.Fatalf("min/max %+v", s)
	}
	if len(s.SampleErrors) != 2 {
		t.Fatalf("sampleErrors %v", s.SampleErrors)
	}
}

func TestShouldAbort(t *testing.T) {
	r := DefaultAbortRules()
	if ok, _ := ShouldAbort(StageSummary{FailRate: 0.6}, r); !ok {
		t.Error("error rate should abort")
	}
	if ok, _ := ShouldAbort(StageSummary{P99Us: 6_000_000}, r); !ok {
		t.Error("p99 should abort")
	}
	if ok, _ := ShouldAbort(StageSummary{FailRate: 0.4, P99Us: 4_000_000}, r); ok {
		t.Error("healthy stage should not abort")
	}
}

func TestComputeVerdict(t *testing.T) {
	mk := func(rps float64, errRate float64, p99 int64) StageSummary {
		return StageSummary{RPS: rps, FailRate: errRate, P99Us: p99}
	}
	// stage2 p99 explodes past 4x baseline
	sums := []StageSummary{mk(100, 0, 1000), mk(400, 0, 1500), mk(500, 0.001, 5000)}
	v := ComputeVerdict(sums, "")
	if v.SaturationStage == nil || *v.SaturationStage != 2 {
		t.Fatalf("saturation = %v", v.SaturationStage)
	}
	if v.MaxHealthyRPS != 400 {
		t.Fatalf("maxHealthyRps = %v", v.MaxHealthyRPS)
	}

	// no saturation, but throughput plateaus while healthy -> generator limited
	sums = []StageSummary{mk(1000, 0, 1000), mk(1050, 0, 1100), mk(1060, 0, 1200)}
	v = ComputeVerdict(sums, "")
	if v.SaturationStage != nil {
		t.Fatal("should not saturate")
	}
	if !v.GeneratorLimited {
		t.Fatal("should flag generator limited")
	}

	// error-rate saturation
	sums = []StageSummary{mk(100, 0, 1000), mk(150, 0.5, 1200)}
	v = ComputeVerdict(sums, "error rate 50%")
	if v.SaturationStage == nil || *v.SaturationStage != 1 || v.AbortReason == "" {
		t.Fatalf("verdict %+v", v)
	}
	fmt.Println(v.Notes) // notes are informational, no assertion
}

func TestRollingP99(t *testing.T) {
	a := NewStageAcc()
	for i := 0; i < 10000; i++ {
		a.Add(Sample{LatencyUs: 100, Code: 200})
	}
	a.Add(Sample{LatencyUs: 99999, Code: 200})
	if p := a.RollingP99Us(); p != 100 { // 1 outlier in last 5000 samples is under p99
		t.Fatalf("rolling p99 = %d", p)
	}
}

func TestJSONTagsStageSummary(t *testing.T) {
	s := StageSummary{
		Stage:        1,
		Requests:     100,
		Concurrency:  10,
		DurationMs:   1000,
		RPS:          50,
		MinUs:        1000,
		MeanUs:       2000,
		MaxUs:        5000,
		P50Us:        1500,
		P90Us:        3000,
		P95Us:        4000,
		P99Us:        4500,
		StartedAtMs:  1700000000000,
		Codes:        map[string]int{"200": 90, "500": 10},
		ErrorCount:   2,
		ErrorRate:    0.02,
		Timeouts:     1,
		SampleErrors: []string{"err1", "err2"},
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal StageSummary failed: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal StageSummary failed: %v", err)
	}

	expectedKeys := map[string]bool{
		"stage":        true,
		"requests":    true,
		"concurrency": true,
		"durationMs":  true,
		"rps":         true,
		"minUs":       true,
		"meanUs":      true,
		"maxUs":       true,
		"p50Us":       true,
		"p90Us":       true,
		"p95Us":       true,
		"p99Us":       true,
		"startedAtMs": true,
		"codes":       true,
		"errorCount":  true,
		"errorRate":   true,
		"failCount":   true,
		"failRate":    true,
		"timeouts":    true,
		"sampleErrors": true,
	}

	for key := range expectedKeys {
		if _, ok := m[key]; !ok {
			t.Errorf("StageSummary: expected key %q not found in JSON", key)
		}
	}

	for key := range m {
		if !expectedKeys[key] {
			t.Errorf("StageSummary: unexpected key %q in JSON", key)
		}
	}

	// Test Verdict JSON tags
	idx := 2
	v := Verdict{
		SaturationStage:  &idx,
		MaxHealthyRPS:    123.4,
		GeneratorLimited: true,
		Notes:            []string{"x"},
		AbortReason:      "y",
	}

	vdata, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal Verdict failed: %v", err)
	}

	var vm map[string]interface{}
	if err := json.Unmarshal(vdata, &vm); err != nil {
		t.Fatalf("json.Unmarshal Verdict failed: %v", err)
	}

	verdictKeys := map[string]bool{
		"saturationStage":  true,
		"maxHealthyRps":    true,
		"generatorLimited": true,
		"notes":            true,
		"abortReason":      true,
	}

	for key := range verdictKeys {
		if _, ok := vm[key]; !ok {
			t.Errorf("Verdict: expected key %q not found in JSON", key)
		}
	}

	for key := range vm {
		if !verdictKeys[key] {
			t.Errorf("Verdict: unexpected key %q in JSON", key)
		}
	}
}

func TestSummarizeZeroSample(t *testing.T) {
	a := NewStageAcc()
	s := a.Summarize(0, Stage{Requests: 0, Concurrency: 1}, 0, time.Time{})

	if s.ErrorRate != 0 {
		t.Errorf("ErrorRate = %v, want 0", s.ErrorRate)
	}
	if s.RPS != 0 {
		t.Errorf("RPS = %v, want 0", s.RPS)
	}
	if s.P99Us != 0 {
		t.Errorf("P99Us = %v, want 0", s.P99Us)
	}
	if s.ErrorCount != 0 {
		t.Errorf("ErrorCount = %v, want 0", s.ErrorCount)
	}
	if s.MinUs != 0 {
		t.Errorf("MinUs = %v, want 0", s.MinUs)
	}
	if s.MaxUs != 0 {
		t.Errorf("MaxUs = %v, want 0", s.MaxUs)
	}
	if s.MeanUs != 0 {
		t.Errorf("MeanUs = %v, want 0", s.MeanUs)
	}
}
