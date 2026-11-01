// Package orch runs a load-test plan stage by stage, streaming progress
// events and persisting results/summaries via the store.
package orch

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"apiperf/curlparse"
	"apiperf/engine"
	"apiperf/metrics"
	"apiperf/store"
)

// Event is a progress/lifecycle notification emitted during a run.
type Event struct {
	Type string `json:"type"` // stage_start | progress | stage_done | run_done
	Data any    `json:"data"`
}

// Orchestrator drives a single load-test run at a time, emitting Events
// and persisting state through st.
type Orchestrator struct {
	st       *store.Store
	emit     func(Event)
	cooldown time.Duration
	timeout  time.Duration
	running  atomic.Bool
	activeID atomic.Int64
	mu       sync.Mutex // guards cancel
	cancel   context.CancelFunc
}

// ErrBusy is returned by Start when a run is already active.
var ErrBusy = errors.New("a run is already in progress")

// New creates an Orchestrator with default cooldown (3s) and timeout (10s).
func New(st *store.Store, emit func(Event)) *Orchestrator {
	return &Orchestrator{
		st:       st,
		emit:     emit,
		cooldown: 3 * time.Second,
		timeout:  10 * time.Second,
	}
}

// SetCooldown overrides the inter-stage cooldown duration.
func (o *Orchestrator) SetCooldown(d time.Duration) { o.cooldown = d }

// SetTimeout overrides the per-request timeout passed to engine.Run.
func (o *Orchestrator) SetTimeout(d time.Duration) { o.timeout = d }

// Start launches the run goroutine. Returns ErrBusy if a run is active.
func (o *Orchestrator) Start(runID int64, tgt curlparse.Target, plan []metrics.Stage, rules metrics.AbortRules) error {
	if !o.running.CompareAndSwap(false, true) {
		return ErrBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	o.mu.Lock()
	o.cancel = cancel
	o.mu.Unlock()
	o.activeID.Store(runID)
	go o.run(ctx, runID, tgt, plan, rules)
	return nil
}

// Stop kills the active run if its id matches runID (no-op when idle or
// when runID doesn't match the active run, e.g. a stale browser tab trying
// to stop a run that already finished and was replaced by a new one).
func (o *Orchestrator) Stop(runID int64) {
	if o.activeID.Load() != runID {
		return
	}
	o.mu.Lock()
	cancel := o.cancel
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Running reports whether a run is active, and its id.
func (o *Orchestrator) Running() (bool, int64) {
	if !o.running.Load() {
		return false, 0
	}
	return true, o.activeID.Load()
}

func (o *Orchestrator) run(ctx context.Context, runID int64, tgt curlparse.Target, plan []metrics.Stage, rules metrics.AbortRules) {
	defer func() {
		o.mu.Lock()
		o.cancel = nil
		o.mu.Unlock()
		o.running.Store(false)
	}()
	if err := o.st.SetStatus(runID, "running"); err != nil {
		log.Printf("orch: SetStatus(running) run=%d: %v", runID, err)
	}
	timeout := o.timeout
	if rules.TimeoutMs > 0 {
		timeout = time.Duration(rules.TimeoutMs) * time.Millisecond
	}
	status, abortReason := "completed", ""
	var sums []metrics.StageSummary
	persistErr := false

stages:
	for i, st := range plan {
		o.emit(Event{"stage_start", map[string]any{"stage": i, "requests": st.Requests, "concurrency": st.Concurrency}})
		acc := metrics.NewStageAcc()
		stageStart := time.Now()
		lastEmit := stageStart
		var buf []metrics.Sample

		for s := range engine.Run(ctx, tgt, st.Requests, st.Concurrency, timeout) {
			acc.Add(s)
			buf = append(buf, s)
			if len(buf) >= 1000 {
				if err := o.st.InsertResults(runID, i, buf); err != nil {
					log.Printf("orch: InsertResults run=%d stage=%d: %v", runID, i, err)
					persistErr = true
				}
				buf = buf[:0]
			}
			if time.Since(lastEmit) >= 500*time.Millisecond {
				lastEmit = time.Now()
				o.emit(Event{"progress", map[string]any{
					"stage": i, "done": acc.Count(), "total": st.Requests,
					"rps":   float64(acc.Count()) / time.Since(stageStart).Seconds(),
					"p99Ms": float64(acc.RollingP99Us()) / 1000,
				}})
			}
		}
		if len(buf) > 0 {
			if err := o.st.InsertResults(runID, i, buf); err != nil {
				log.Printf("orch: InsertResults run=%d stage=%d: %v", runID, i, err)
				persistErr = true
			}
		}
		sum := acc.Summarize(i, st, time.Since(stageStart), stageStart)
		if err := o.st.SaveStageSummary(runID, sum); err != nil {
			log.Printf("orch: SaveStageSummary run=%d stage=%d: %v", runID, i, err)
			persistErr = true
		}
		o.emit(Event{"stage_done", sum})
		sums = append(sums, sum)

		if ctx.Err() != nil {
			status = "killed"
			break stages
		}
		if abort, reason := metrics.ShouldAbort(sum, rules); abort {
			status, abortReason = "aborted", reason
			break stages
		}
		if i < len(plan)-1 {
			select {
			case <-ctx.Done():
				status = "killed"
				break stages
			case <-time.After(o.cooldown):
			}
		}
	}
	v := metrics.ComputeVerdict(sums, abortReason)
	if persistErr {
		v.Notes = append(v.Notes, "warning: some results/summaries failed to persist to storage — this report may reflect incomplete data")
	}
	if err := o.st.FinishRun(runID, status, v); err != nil {
		log.Printf("orch: FinishRun run=%d: %v", runID, err)
	}
	o.emit(Event{"run_done", map[string]any{"status": status, "verdict": v}})
}
