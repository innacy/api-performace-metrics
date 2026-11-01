package metrics

import (
	"fmt"
	"math"
	"slices"
	"time"
)

type Sample struct {
	OffsetMs  int64  // ms since stage start
	LatencyUs int64
	Code      int    // 0 when transport error
	Error     string // "" when none
	Timeout   bool
}

type Stage struct {
	Requests    int `json:"requests"`
	Concurrency int `json:"concurrency"`
}

type AbortRules struct {
	MaxErrorRate float64 `json:"maxErrorRate"`        // default 0.5; compared to FailRate (errors + 5xx)
	MaxP99Ms     int64   `json:"maxP99Ms"`            // default 5000
	TimeoutMs    int64   `json:"timeoutMs,omitempty"` // per-request timeout; 0 = orchestrator default
}

type StageSummary struct {
	Stage        int            `json:"stage"`
	Requests     int            `json:"requests"`
	Concurrency  int            `json:"concurrency"`
	DurationMs   int64          `json:"durationMs"`
	RPS          float64        `json:"rps"`
	MinUs        int64          `json:"minUs"`
	MeanUs       int64          `json:"meanUs"`
	MaxUs        int64          `json:"maxUs"`
	P50Us        int64          `json:"p50Us"`
	P90Us        int64          `json:"p90Us"`
	P95Us        int64          `json:"p95Us"`
	P99Us        int64          `json:"p99Us"`
	StartedAtMs  int64          `json:"startedAtMs"` // Unix ms (UTC) when the stage began — lets the UI plot real wall-clock time
	Codes        map[string]int `json:"codes"`      // "200": 950
	ErrorCount   int            `json:"errorCount"` // transport errors + timeouts
	ErrorRate    float64        `json:"errorRate"`  // errors / total (5xx do NOT count as errors here; they show in Codes)
	FailCount    int            `json:"failCount"`  // errors + 5xx responses
	FailRate     float64        `json:"failRate"`   // failCount / total; drives abort + saturation
	Timeouts     int            `json:"timeouts"`
	SampleErrors []string       `json:"sampleErrors"` // first 5 distinct error strings
}

type Verdict struct {
	SaturationStage  *int     `json:"saturationStage"` // nil = never saturated
	MaxHealthyRPS    float64  `json:"maxHealthyRps"`
	GeneratorLimited bool     `json:"generatorLimited"`
	Notes            []string `json:"notes"`
	AbortReason      string   `json:"abortReason,omitempty"`
}

func DefaultPlan() []Stage {
	return []Stage{{100, 10}, {1000, 25}, {10000, 50}, {100000, 100}, {1000000, 250}}
}

func DefaultAbortRules() AbortRules { return AbortRules{MaxErrorRate: 0.5, MaxP99Ms: 5000} }

func Percentile(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// StageAcc accumulates one stage's samples. Not safe for concurrent use; the caller must serialize calls to Add.
type StageAcc struct {
	latencies    []int64
	codes        map[int]int
	errs         int
	fails        int
	timeouts     int
	sampleErrors []string
	seenErrors   map[string]bool
}

func NewStageAcc() *StageAcc {
	return &StageAcc{codes: map[int]int{}, seenErrors: map[string]bool{}}
}

func (a *StageAcc) Add(s Sample) {
	a.latencies = append(a.latencies, s.LatencyUs)
	if s.Code != 0 {
		a.codes[s.Code]++
		if s.Code >= 500 {
			a.fails++
		}
		return
	}
	// Code == 0: genuine transport failure/timeout, not an HTTP response.
	a.errs++
	a.fails++
	if s.Timeout {
		a.timeouts++
	}
	if s.Error != "" && len(a.sampleErrors) < 5 && !a.seenErrors[s.Error] {
		a.seenErrors[s.Error] = true
		a.sampleErrors = append(a.sampleErrors, s.Error)
	}
}

func (a *StageAcc) Count() int { return len(a.latencies) }

func (a *StageAcc) RollingP99Us() int64 {
	n := len(a.latencies)
	win := 5000
	if n < win {
		win = n
	}
	recent := slices.Clone(a.latencies[n-win:])
	slices.Sort(recent)
	return Percentile(recent, 0.99)
}

func (a *StageAcc) Summarize(stageIdx int, st Stage, wall time.Duration, startedAt time.Time) StageSummary {
	sorted := slices.Clone(a.latencies)
	slices.Sort(sorted)
	total := len(sorted)
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	s := StageSummary{
		Stage: stageIdx, Requests: st.Requests, Concurrency: st.Concurrency,
		DurationMs: wall.Milliseconds(), StartedAtMs: startedAt.UnixMilli(),
		Codes:      map[string]int{}, ErrorCount: a.errs, FailCount: a.fails, Timeouts: a.timeouts,
		SampleErrors: a.sampleErrors,
	}
	if total > 0 {
		s.MinUs, s.MaxUs = sorted[0], sorted[total-1]
		s.MeanUs = sum / int64(total)
		s.P50Us = Percentile(sorted, 0.50)
		s.P90Us = Percentile(sorted, 0.90)
		s.P95Us = Percentile(sorted, 0.95)
		s.P99Us = Percentile(sorted, 0.99)
		s.ErrorRate = float64(a.errs) / float64(total)
		s.FailRate = float64(a.fails) / float64(total)
	}
	if wall > 0 {
		s.RPS = float64(total) / wall.Seconds()
	}
	for code, n := range a.codes {
		s.Codes[fmt.Sprintf("%d", code)] = n
	}
	return s
}

func ShouldAbort(s StageSummary, r AbortRules) (bool, string) {
	if s.FailRate > r.MaxErrorRate {
		return true, fmt.Sprintf("failure rate %.0f%% exceeds %.0f%%", s.FailRate*100, r.MaxErrorRate*100)
	}
	if s.P99Us > r.MaxP99Ms*1000 {
		return true, fmt.Sprintf("p99 %.1fs exceeds %.1fs", float64(s.P99Us)/1e6, float64(r.MaxP99Ms)/1e3)
	}
	return false, ""
}

func ComputeVerdict(sums []StageSummary, abortReason string) Verdict {
	v := Verdict{AbortReason: abortReason}
	if len(sums) == 0 {
		return v
	}
	baseP99 := sums[0].P99Us
	for i, s := range sums {
		if s.FailRate > 0.01 || (i > 0 && baseP99 > 0 && s.P99Us >= 4*baseP99) {
			idx := i
			v.SaturationStage = &idx
			v.Notes = append(v.Notes, fmt.Sprintf(
				"degradation first observed at stage %d (%d requests, concurrency %d)",
				i+1, s.Requests, s.Concurrency))
			break
		}
	}
	healthy := sums
	if v.SaturationStage != nil {
		healthy = sums[:*v.SaturationStage]
	}
	for _, s := range healthy {
		if s.RPS > v.MaxHealthyRPS {
			v.MaxHealthyRPS = s.RPS
		}
	}
	for i := 1; i < len(healthy); i++ {
		prev, cur := healthy[i-1], healthy[i]
		if cur.RPS < prev.RPS*1.10 && cur.FailRate <= 0.01 &&
			(prev.P99Us == 0 || cur.P99Us < prev.P99Us*3/2) {
			v.GeneratorLimited = true
			v.Notes = append(v.Notes,
				"throughput plateaued while the API stayed healthy — the load generator (this machine) may be the bottleneck; results above this RPS reflect client limits, not the API")
			break
		}
	}
	if v.SaturationStage == nil && !v.GeneratorLimited {
		v.Notes = append(v.Notes, "no degradation detected across all completed stages")
	}
	return v
}
