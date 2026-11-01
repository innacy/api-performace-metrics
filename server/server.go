package server

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"apiperf/curlparse"
	"apiperf/engine"
	"apiperf/metrics"
	"apiperf/orch"
	"apiperf/store"
)

//go:embed ui
var uiFS embed.FS

const (
	probeTimeout  = 10 * time.Second
	seriesBuckets = 60 // per stage; the report draws all stages side by side
	histogramBins = 60
)

// Hub fans out orchestrator events to SSE subscribers.
type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// NewHub creates an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[chan []byte]struct{})}
}

// Emit marshals e and broadcasts it to all subscribers. Sends are
// non-blocking: a slow subscriber's message is dropped rather than
// blocking the emitter.
func (h *Hub) Emit(e orch.Event) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- b:
		default: // slow client: drop
		}
	}
}

// Subscribe registers a new subscriber and returns its channel plus a
// cancel func to unregister it. cancel is safe to call more than once.
func (h *Hub) Subscribe() (ch chan []byte, cancel func()) {
	ch = make(chan []byte, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel = func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
		})
	}
	return ch, cancel
}

// pendingRun caches what Start needs for a run created but not yet started.
type pendingRun struct {
	curl     string
	target   curlparse.Target
	plan     []metrics.Stage
	rules    metrics.AbortRules
	external bool
}

// maxStageConcurrency caps per-stage concurrency to keep a single run from
// spawning enough goroutines/connections to OOM the process.
const maxStageConcurrency = 10000

// maxTimeoutMs caps the per-request timeout a run may ask for.
const maxTimeoutMs = 120_000

// validatePlan rejects stage plans that would hang or blow up the engine:
// an empty plan, or any stage with requests/concurrency out of bounds.
func validatePlan(plan []metrics.Stage) error {
	if len(plan) == 0 {
		return fmt.Errorf("stages must not be empty")
	}
	for i, st := range plan {
		if st.Requests < 1 {
			return fmt.Errorf("stage %d: requests must be >= 1, got %d", i+1, st.Requests)
		}
		if st.Concurrency < 1 || st.Concurrency > maxStageConcurrency {
			return fmt.Errorf("stage %d: concurrency must be between 1 and %d, got %d", i+1, maxStageConcurrency, st.Concurrency)
		}
	}
	return nil
}

type server struct {
	st      *store.Store
	o       *orch.Orchestrator
	hub     *Hub
	pmu     sync.Mutex
	pending map[int64]pendingRun
}

// New builds the HTTP handler: embedded UI, REST API, and SSE hub.
func New(st *store.Store, o *orch.Orchestrator, hub *Hub) http.Handler {
	s := &server{st: st, o: o, hub: hub, pending: make(map[int64]pendingRun)}

	mux := http.NewServeMux()
	ui, _ := fs.Sub(uiFS, "ui")
	// Embedded files have no mtime, so browsers can't tell a new build's UI
	// apart from a cached one: make them revalidate every load.
	files := http.FileServer(http.FS(ui))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})

	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /api/runs", s.handleCreateRun)
	mux.HandleFunc("POST /api/runs/{id}/start", s.handleStartRun)
	mux.HandleFunc("POST /api/runs/{id}/stop", s.handleStopRun)
	mux.HandleFunc("GET /api/runs", s.handleListRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /api/runs/{id}/series", s.handleSeries)
	mux.HandleFunc("GET /api/runs/{id}/hist", s.handleHist)
	mux.HandleFunc("GET /api/events", s.handleEvents)

	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, err := json.Marshal(v)
	if err != nil {
		errBody, _ := json.Marshal(map[string]any{"error": "failed to encode response: " + err.Error()})
		w.WriteHeader(http.StatusInternalServerError)
		w.Write(errBody)
		return
	}
	w.WriteHeader(status)
	w.Write(b)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// pathInt64 parses the {id} path value; ok is false on malformed input.
func pathInt64(r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// decodeOptionalJSON decodes r.Body into v. A missing/empty body is treated
// as a no-op (v keeps its zero value); malformed JSON returns an error.
func decodeOptionalJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

type createRunRequest struct {
	Curl   string              `json:"curl"`
	Stages []metrics.Stage     `json:"stages"`
	Rules  *metrics.AbortRules `json:"rules"`
}

type createRunResponse struct {
	RunID    int64              `json:"runId"`
	Probe    engine.ProbeResult `json:"probe"`
	External bool               `json:"external"`
	Plan     []metrics.Stage    `json:"plan"`
	Rules    metrics.AbortRules `json:"rules"`
	Target   curlparse.Target   `json:"target"`
}

func (s *server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	tgt, err := curlparse.Parse(req.Curl)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	plan := req.Stages
	if plan == nil {
		plan = metrics.DefaultPlan()
	}
	if err := validatePlan(plan); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rules := metrics.DefaultAbortRules()
	if req.Rules != nil {
		rules = *req.Rules
	}
	if rules.TimeoutMs < 0 || rules.TimeoutMs > maxTimeoutMs {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("timeoutMs must be between 1 and %d", maxTimeoutMs))
		return
	}

	probe := engine.Probe(tgt, probeTimeout)
	external := !engine.IsPrivateHost(tgt.URL)

	runID, err := s.st.CreateRun(req.Curl, tgt, plan, rules)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create run: "+err.Error())
		return
	}
	if err := s.st.SetProbe(runID, probe); err != nil {
		log.Printf("server: SetProbe run=%d: %v", runID, err) // report just lacks the timing section
	}

	s.pmu.Lock()
	// Evict any earlier pending entry for the same curl text: the UI
	// re-creates a run when the user edits the plan/rules after Verify,
	// which would otherwise orphan the first pending entry (and its Target,
	// including body) for the process lifetime.
	for id, pr := range s.pending {
		if pr.curl == req.Curl {
			delete(s.pending, id)
		}
	}
	s.pending[runID] = pendingRun{curl: req.Curl, target: tgt, plan: plan, rules: rules, external: external}
	s.pmu.Unlock()

	writeJSON(w, http.StatusCreated, createRunResponse{
		RunID:    runID,
		Probe:    probe,
		External: external,
		Plan:     plan,
		Rules:    rules,
		Target:   tgt,
	})
}

type startRunRequest struct {
	ConfirmExternal bool `json:"confirmExternal"`
}

func (s *server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathInt64(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}

	var req startRunRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	s.pmu.Lock()
	pr, found := s.pending[runID]
	s.pmu.Unlock()
	if !found {
		// No pending entry: either the run id never existed, or it did and
		// was already started (and thus popped from pending on success).
		// Distinguish the two so a completed/started run can't be silently
		// re-run (which would duplicate its results rows).
		if _, err := s.st.GetRun(runID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, "unknown run id")
				return
			}
			log.Printf("server: GetRun run=%d: %v", runID, err)
			writeError(w, http.StatusInternalServerError, "failed to look up run")
			return
		}
		writeError(w, http.StatusConflict, "run already started")
		return
	}

	if pr.external && !req.ConfirmExternal {
		writeError(w, http.StatusForbidden, "target is not a private host; confirm to proceed")
		return
	}

	if err := s.o.Start(runID, pr.target, pr.plan, pr.rules); err != nil {
		if errors.Is(err, orch.ErrBusy) {
			writeError(w, http.StatusConflict, "a run is already in progress")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.pmu.Lock()
	delete(s.pending, runID)
	s.pmu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"started": true})
}

func (s *server) handleStopRun(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathInt64(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	s.o.Stop(runID)
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true})
}

func (s *server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.st.ListRuns()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list runs: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathInt64(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	d, err := s.st.GetRun(runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		log.Printf("server: GetRun run=%d: %v", runID, err)
		writeError(w, http.StatusInternalServerError, "failed to get run")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) handleSeries(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathInt64(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	stage, ok := parseStageParam(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid stage parameter")
		return
	}
	pts, err := s.st.Series(runID, stage, seriesBuckets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to compute series: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pts)
}

func (s *server) handleHist(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathInt64(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	stage, ok := parseStageParam(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid stage parameter")
		return
	}
	bars, err := s.st.Histogram(runID, stage, histogramBins)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to compute histogram: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, bars)
}

func parseStageParam(r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("stage")
	if raw == "" {
		return 0, true
	}
	stage, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return stage, true
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	ch, cancel := s.hub.Subscribe()
	defer cancel()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			fl.Flush()
		}
	}
}
