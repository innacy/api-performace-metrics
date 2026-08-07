# API Performance Metrics Calculator — Design

**Date:** 2026-08-07
**Status:** Approved by user (brainstorming session)

## Purpose

A local tool that tells you everything about an API's performance from a single pasted curl command. It runs a staged load ramp (100 → 1M requests with rising concurrency), stores every response time, computes exact percentiles per stage, detects the saturation point, and presents it all in a live local dashboard with run history.

Primary targets: the user's own APIs (local/dev and staging/prod they operate). Arbitrary third-party APIs are supported but guarded (see Error Handling).

## Approach Decision

Evaluated against existing open source (Fortio, Plow, k6, Locust, vegeta, hey, oha). Nothing covers the full loop: paste curl → verify → staged ramp → auto-detect saturation → stored history + live dashboard. The request engine, however, is a solved problem.

- **Chosen: wrap the [vegeta](https://github.com/tsenart/vegeta) Go library as the engine.** All our code is the differentiating layer (curl parsing, ramp orchestration, storage, dashboard); firing requests accurately at high concurrency is vegeta's solved problem. Vegeta's `rate=0, max-workers=N` mode gives closed-loop "concurrency N, as fast as possible" — matching the user's mental model.
- Rejected: own net/http worker-pool engine (re-learns transport/keepalive/timeout edge cases at 1M scale) and shelling out to k6/oha (no clean per-request stream for the live UI, external binary, curl→script conversion). The engine sits behind one small interface so a homegrown engine remains a drop-in swap later.

## Architecture

One Go binary (`apiperf`). Run it, open `http://localhost:7071`.

```
Browser (vanilla HTML/JS + vendored uPlot)
   │  REST + SSE
   ▼
Go binary
 ├── HTTP server (net/http, UI via go:embed)
 ├── curl parser        → Target{method, url, headers, body}
 ├── ramp orchestrator  → sequential stages, abort logic, kill switch
 ├── vegeta attacker    → rate=0 + max-workers=C, stop after N requests
 └── SQLite store       → runs / stage summaries / every response time
```

- **No frontend toolchain:** vanilla JS, uPlot vendored (~50KB, handles million-point series).
- **SQLite via `modernc.org/sqlite`** (pure Go, no cgo) with WAL + batch inserts.
- **SSE** (not WebSocket) for live updates — one-way is all that's needed.

## Components

### Curl parser
Shell-lexes the pasted command. v1 supports: URL, `-X/--request`, `-H/--header`, `-d/--data/--data-raw/--data-binary`, `-u/--user`, `-k/--insecure`. Unsupported flags → hard reject with "flag X not supported yet" (trust boundary; never silently ignore).

### Verify probe
One real request before any load: shows status, latency, response size/snippet in the UI. Non-2xx warns but allows proceeding (testing a 401 path is legitimate).

### Stage plan
Editable table in the UI; defaults:

| Requests | Concurrency |
|---|---|
| 100 | 10 |
| 1,000 | 25 |
| 10,000 | 50 |
| 100,000 | 100 |
| 1,000,000 | 250 |

### Orchestrator
Stages run sequentially with a short cooldown between them. After each stage: compute summary → evaluate abort rule (default: error rate > 50% OR p99 > 5s; both editable) → emit stage result over SSE → continue or stop with reason. Kill switch = context cancel; current stage stops within ~1s; partial stage data kept and marked. One run at a time; concurrent start returns 409.

### Metrics
Every latency is stored, so percentiles are **exact** (sort, index) — no histogram estimation. Per stage: p50/p90/p95/p99, min/max/mean, status-code counts, error/timeout counts, achieved RPS. **Saturation point:** first stage where error rate > 1% OR p99 > 4× baseline (stage 1) p99; reported as "handles ~X req/s before degrading."

### Store (SQLite)
- `runs`: curl command, stage plan, timestamps, status (completed/aborted/killed), verdict/saturation analysis.
- `stage_summaries`: per-stage computed metrics.
- `results`: run ID, stage, offset-ms from stage start, latency-µs, status code, error string.

A full 1M ramp ≈ 1.1M rows ≈ ~40MB. History page lists past runs; any run reopens fully.

### Live dashboard (single page)
Curl input → verify card → editable stage plan → Start. During run: stage table fills live (progress, RPS, rolling p99) via SSE; red Stop button. Per stage on completion: latency-over-time chart, histogram, and a run-level latency-vs-load curve. After run: full report view (same view history opens).

## Data Flow

1. `POST /api/runs` `{curl, stages, abortRules}` → parse curl (400 with exact reason on failure) → verify probe → return probe result + run ID. Run starts only on explicit UI confirm.
2. Orchestrator goroutine: per stage, vegeta attacker (`rate=0, maxWorkers=C`, stop after N).
3. Result channel fans out to: **batcher** (1k-row SQLite batch inserts) and **live aggregator** (rolling count/RPS/p99 over SSE every ~500ms).
4. Stage end → exact summary from stored latencies → `stage_summaries` → SSE push → abort check → cooldown → next stage.
5. Run end → verdict + saturation analysis → `runs` → UI shows report. History: `GET /api/runs`, `GET /api/runs/:id`, `GET /api/runs/:id/series?stage=n` (server-side downsample to ~2k points for charts).

## Error Handling

- **Verify fails** (DNS/unreachable/timeout): run never starts; exact error shown.
- **Mid-stage collapse:** errors are data — recorded per request; abort rule ends the run gracefully ("aborted at stage 4: error rate 82%").
- **Generator-side bottleneck:** at high concurrency the client can saturate (fds, ephemeral ports, CPU). Raise limits programmatically where possible; warn in the report when achieved RPS looks generator-limited; document `ulimit -n`. Known v1 ceiling — detected and reported honestly, not hidden.
- **Kill switch / Ctrl-C:** context cancellation throughout; partial data flushed; run marked `killed`, viewable in history.
- **Non-private hostname guardrail:** targets resolving outside private/loopback ranges get a UI confirmation warning before anything above stage 1 runs — prevents accidentally load-testing someone else's prod.

## Testing

- **Unit:** curl parser (table-driven), exact-percentile math against fixtures, abort-rule and saturation-detection logic.
- **Integration:** `httptest` server with injectable latency/errors; tiny ramp (100@2, 500@5) through the real orchestrator + SQLite; assert summaries, abort triggering, kill-switch cancellation. Seconds to run, no network.
- **Not tested:** vegeta internals; UI beyond an embedded-assets smoke check.

## Out of Scope (v1)

- Multiple input sources (HAR, Postman, OpenAPI) — curl only; input layer designed so more sources slot in later.
- Distributed / multi-machine load generation.
- Run-vs-run comparison view (history stores everything needed; UI later).
- Auth token refresh mid-run.
