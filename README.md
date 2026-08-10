# apiperf

Paste a curl command, get the full performance story of your API: a staged
load ramp (100 → 1M requests, rising concurrency), exact p50/p90/p95/p99 per
stage, error breakdown, throughput, saturation-point detection, live
dashboard, kill switch, and run history — from one Go binary.

## Run

    go build -o apiperf . && ./apiperf
    # open http://localhost:7071

Paste your curl (supported flags: -X, -H, -d/--data*, -u, -k, -L, --url), hit Verify,
tune the stage plan if you like, Start. Results stream live; the final
report includes charts and a verdict. Every response time is stored in
`apiperf.db` (SQLite); the History tab reopens any past run.

## Notes

- Auto-abort: a run stops early when a stage exceeds the error-rate or p99
  thresholds (editable in the UI).
- Only load-test APIs you own or have permission to test. Non-private hosts
  require an explicit confirmation.
- At high concurrency the generator machine can be the bottleneck; the
  verdict flags this when throughput plateaus while the API stays healthy.
  Raise `ulimit -n` if you see file-descriptor errors (the binary attempts
  to raise its own soft limit at startup).
- A full 1M-request ramp stores ~1.1M rows (~40 MB) per run.
