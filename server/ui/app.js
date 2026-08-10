'use strict';
const $ = (s) => document.querySelector(s);
const fmtUs = (us) => us >= 1e6 ? (us / 1e6).toFixed(2) + 's' : us >= 1000 ? (us / 1000).toFixed(1) + 'ms' : us + 'µs';

const state = { runId: null, external: false, plan: [], summaries: [] };
const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;');

// Theme toggle: persisted in localStorage, defaults to light
function applyTheme(theme) {
  document.documentElement.classList.toggle('dark', theme === 'dark');
  document.querySelectorAll('#theme-toggle button').forEach(b =>
    b.classList.toggle('active', b.dataset.theme === theme));
  localStorage.setItem('apiperf-theme', theme);
}
applyTheme(localStorage.getItem('apiperf-theme') || 'light');
document.querySelectorAll('#theme-toggle button').forEach(b =>
  b.onclick = () => applyTheme(b.dataset.theme));

function show(view) {
  for (const v of ['setup', 'live', 'report', 'history']) $('#view-' + v).classList.toggle('hidden', v !== view);
}
document.querySelectorAll('nav a').forEach(a => a.onclick = () => {
  if (a.dataset.view === 'history') loadHistory();
  show(a.dataset.view);
});

function renderPlanTable(plan) {
  $('#plan-table tbody').innerHTML = plan.map((s, i) =>
    `<tr><td>${i + 1}</td>
     <td><input type="number" class="req" value="${s.requests}" min="1"></td>
     <td><input type="number" class="conc" value="${s.concurrency}" min="1"></td></tr>`).join('');
}

// clampMin1 coerces a raw input value to a finite integer >= 1, so a blank
// or garbage field (e.g. an empty concurrency box) can't submit a plan that
// hangs the engine (concurrency 0) or blows it up (negative concurrency).
function clampMin1(raw) {
  const n = Math.floor(Number(raw));
  return Number.isFinite(n) && n >= 1 ? n : 1;
}

function readPlan() {
  const reqs = [...document.querySelectorAll('#plan-table .req')].map(i => clampMin1(i.value));
  const concs = [...document.querySelectorAll('#plan-table .conc')].map(i => clampMin1(i.value));
  return reqs.map((r, i) => ({ requests: r, concurrency: concs[i] }));
}

function currentRules() {
  return { maxErrorRate: +$('#rule-err').value, maxP99Ms: +$('#rule-p99').value };
}

$('#btn-verify').onclick = async () => {
  $('#verify-err').textContent = '';
  const res = await fetch('/api/runs', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ curl: $('#curl').value }),
  });
  const body = await res.json();
  if (!res.ok) { $('#verify-err').textContent = body.error; return; }
  state.runId = body.runId;
  state.external = body.external;
  state.plan = body.plan;
  const p = body.probe;
  $('#probe-body').innerHTML = p.error
    ? `<span class="bad">Probe failed: ${esc(p.error)}</span> — fix the target before starting.`
    : `<b class="${p.status < 400 ? 'good' : 'bad'}">HTTP ${p.status}</b> in ${p.latencyMs}ms, ${p.bytes} bytes
       <pre class="muted" style="white-space:pre-wrap">${esc((p.snippet || '').slice(0, 300))}</pre>`;
  $('#warn-external').classList.toggle('hidden', !body.external);
  renderPlanTable(body.plan.map(s => ({ requests: s.requests, concurrency: s.concurrency })));
  $('#probe-card').classList.remove('hidden');
  $('#btn-start').disabled = !!p.error;
  // Snapshot what was just verified. Start reuses this run as-is unless the
  // curl text, plan, or rules are edited before Start is clicked — avoids
  // firing a second probe / creating a second (orphaned) run per test.
  state.verifiedCurl = $('#curl').value;
  state.verifiedPlan = JSON.stringify(readPlan());
  state.verifiedRules = JSON.stringify(currentRules());
};

$('#btn-start').onclick = async () => {
  const curl = $('#curl').value;
  const plan = readPlan();
  const rules = currentRules();
  const unchanged = state.runId != null && curl === state.verifiedCurl &&
    JSON.stringify(plan) === state.verifiedPlan && JSON.stringify(rules) === state.verifiedRules;

  // Re-creates the run from what's currently on screen. The server evicts
  // any superseded pending entry for this curl.
  async function createRun() {
    const res = await fetch('/api/runs', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ curl, stages: plan, rules }),
    });
    const body = await res.json();
    if (!res.ok) throw new Error(body.error || 'failed to create run');
    state.runId = body.runId; state.plan = body.plan || plan;
  }

  try {
    if (!unchanged) {
      // curl/plan/rules changed since Verify (or Verify was never run for
      // this exact config, or the previously verified run already finished).
      await createRun();
    } else {
      state.plan = plan;
    }
  } catch (e) {
    $('#verify-err').textContent = e.message;
    return;
  }

  state.summaries = [];
  // Connect SSE and wait for the connection to actually open (or a 2s
  // timeout) before starting the run, so no early events (e.g. the first
  // stage_start) are missed to the EventSource handshake racing the POST.
  initLiveTable(state.plan);
  await connectSSE();
  show('live');

  let start = await fetch(`/api/runs/${state.runId}/start`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ confirmExternal: $('#confirm-external').checked }),
  });
  if (start.status === 409) {
    // Belt-and-braces: the run id we tried to start was already
    // started/finished (a stale snapshot slipped through). Mint a fresh
    // run for what's on screen and retry once.
    try {
      await createRun();
    } catch (e) {
      if (es) es.close();
      $('#verify-err').textContent = e.message;
      show('setup');
      return;
    }
    start = await fetch(`/api/runs/${state.runId}/start`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ confirmExternal: $('#confirm-external').checked }),
    });
  }
  if (!start.ok) {
    if (es) es.close();
    $('#verify-err').textContent = (await start.json()).error;
    show('setup');
    return;
  }
};

$('#btn-stop').onclick = async () => {
  const res = await fetch(`/api/runs/${state.runId}/stop`, { method: 'POST' });
  if (!res.ok) { $('#live-status').textContent = (await res.json()).error; }
};

function initLiveTable(plan) {
  $('#live-table tbody').innerHTML = plan.map((s, i) =>
    `<tr id="stage-${i}"><td>${i + 1}</td><td>${s.requests.toLocaleString()}</td><td>${s.concurrency}</td>
     <td class="c-prog">–</td><td class="c-rps">–</td><td class="c-p50">–</td><td class="c-p90">–</td>
     <td class="c-p95">–</td><td class="c-p99">–</td><td class="c-err">–</td><td class="c-status muted">waiting</td></tr>`).join('');
  $('#live-status').textContent = '';
}

let es = null;
function connectSSE() {
  if (es) es.close();
  es = new EventSource('/api/events');
  es.onmessage = (m) => {
    const ev = JSON.parse(m.data);
    const d = ev.data;
    if (ev.type === 'stage_start') {
      const row = $(`#stage-${d.stage}`);
      if (!row) return;
      row.querySelector('.c-status').textContent = 'running';
    } else if (ev.type === 'progress') {
      const row = $(`#stage-${d.stage}`);
      if (!row) return;
      row.querySelector('.c-prog').textContent = `${Math.round(100 * d.done / d.total)}%`;
      row.querySelector('.c-rps').textContent = d.rps.toFixed(0);
      row.querySelector('.c-p99').textContent = d.p99Ms.toFixed(1) + 'ms';
    } else if (ev.type === 'stage_done') {
      state.summaries.push(d);
      const row = $(`#stage-${d.stage}`);
      if (!row) return;
      row.querySelector('.c-prog').textContent = '100%';
      row.querySelector('.c-rps').textContent = d.rps.toFixed(0);
      row.querySelector('.c-p50').textContent = fmtUs(d.p50Us);
      row.querySelector('.c-p90').textContent = fmtUs(d.p90Us);
      row.querySelector('.c-p95').textContent = fmtUs(d.p95Us);
      row.querySelector('.c-p99').textContent = fmtUs(d.p99Us);
      row.querySelector('.c-err').innerHTML = d.errorCount
        ? `<span class="bad">${(d.errorRate * 100).toFixed(1)}%</span>` : '<span class="good">0</span>';
      row.querySelector('.c-status').textContent = 'done';
    } else if (ev.type === 'run_done') {
      es.close();
      const finishedId = state.runId;
      $('#live-status').textContent = `Run ${d.status}.` + (d.verdict && d.verdict.abortReason ? ` Reason: ${d.verdict.abortReason}` : '');
      loadReport(finishedId);
      // Clear the verify snapshot: this run is now finished, so the next
      // Start (even with the curl/plan/rules totally unchanged) must mint
      // a fresh run instead of trying to re-start this one.
      state.runId = null;
      state.verifiedCurl = null;
      state.verifiedPlan = null;
      state.verifiedRules = null;
    }
  };
  return new Promise((resolve) => {
    const done = () => resolve();
    es.onopen = done;
    setTimeout(done, 2000);
  });
}

async function loadReport(id) {
  const d = await (await fetch(`/api/runs/${id}`)).json();
  renderReport(d);
  show('report');
}

function verdictHTML(d) {
  const v = d.verdict || {};
  const parts = [];
  if (v.abortReason) parts.push(`<p class="bad">Aborted: ${esc(v.abortReason)}</p>`);
  if (v.saturationStage != null) {
    const s = d.summaries[v.saturationStage];
    parts.push(`<p class="bad">Saturation: degradation begins at stage ${v.saturationStage + 1} (${s.requests.toLocaleString()} req @ ${s.concurrency} conc).</p>`);
  }
  if (v.maxHealthyRps) parts.push(`<p class="good">Handles ~${v.maxHealthyRps.toFixed(0)} req/s before degrading.</p>`);
  for (const n of v.notes || []) parts.push(`<p class="muted">${esc(n)}</p>`);
  return parts.join('');
}

function summaryTableHTML(d) {
  return `<table><thead><tr><th>Stage</th><th>Requests</th><th>Conc</th><th>RPS</th><th>p50</th><th>p90</th><th>p95</th><th>p99</th><th>min</th><th>max</th><th>Codes</th><th>Errors</th></tr></thead><tbody>` +
    d.summaries.map(s => `<tr><td>${s.stage + 1}</td><td>${s.requests.toLocaleString()}</td><td>${s.concurrency}</td>
      <td>${s.rps.toFixed(0)}</td><td>${fmtUs(s.p50Us)}</td><td>${fmtUs(s.p90Us)}</td><td>${fmtUs(s.p95Us)}</td>
      <td>${fmtUs(s.p99Us)}</td><td>${fmtUs(s.minUs)}</td><td>${fmtUs(s.maxUs)}</td>
      <td class="muted">${Object.entries(s.codes || {}).map(([c, n]) => `${c}:${n}`).join(' ')}</td>
      <td>${s.errorCount ? `<span class="bad">${(s.errorRate * 100).toFixed(1)}%</span>` : '0'}</td></tr>`).join('') +
    `</tbody></table>`;
}

function chartColors() {
  const s = getComputedStyle(document.documentElement);
  return {
    grid: s.getPropertyValue('--chart-grid').trim(),
    axis: s.getPropertyValue('--chart-axis').trim(),
    fg:   s.getPropertyValue('--fg').trim(),
    muted:s.getPropertyValue('--muted').trim(),
    c1:   s.getPropertyValue('--c1').trim(),  c1f: s.getPropertyValue('--c1f').trim(),
    c2:   s.getPropertyValue('--c2').trim(),  c2f: s.getPropertyValue('--c2f').trim(),
    c3:   s.getPropertyValue('--c3').trim(),  c3f: s.getPropertyValue('--c3f').trim(),
    c4:   s.getPropertyValue('--c4').trim(),  c4f: s.getPropertyValue('--c4f').trim(),
    bar1: s.getPropertyValue('--bar1').trim(), bar2: s.getPropertyValue('--bar2').trim(),
    bar3: s.getPropertyValue('--bar3').trim(), bar4: s.getPropertyValue('--bar4').trim(),
  };
}

function gradientFill(color) {
  return (u, seriesIdx) => {
    const top = u.valToPos(u.scales.y.max, 'y', true);
    const bot = u.valToPos(u.scales.y.min, 'y', true);
    if (!isFinite(top) || !isFinite(bot) || top === bot) return color;
    const ctx = u.ctx;
    const grad = ctx.createLinearGradient(0, top, 0, bot);
    grad.addColorStop(0, color);
    grad.addColorStop(1, 'transparent');
    return grad;
  };
}

function modernSeries(raw) {
  const cc = chartColors();
  const palette = [[cc.c2, cc.c2f], [cc.c4, cc.c4f], [cc.c1, cc.c1f], [cc.c3, cc.c3f]];
  return raw.map((s, i) => {
    if (i === 0) return s;
    const [stroke, fill] = palette[(i - 1) % palette.length];
    return {
      ...s,
      stroke: s.stroke || stroke,
      width: s.width != null ? s.width : 2,
      fill: s.fill !== undefined ? s.fill : gradientFill(fill),
      points: s.points || { show: true, size: 5, fill: s.stroke || stroke, stroke: s.stroke || stroke },
    };
  });
}

function modernAxes() {
  const cc = chartColors();
  return [
    { stroke: cc.axis, grid: { stroke: cc.grid, width: 1 }, ticks: { stroke: cc.grid, width: 1 }, font: '11px system-ui', labelFont: '11px system-ui' },
    { stroke: cc.axis, grid: { stroke: cc.grid, width: 1 }, ticks: { stroke: cc.grid, width: 1 }, font: '11px system-ui', labelFont: '11px system-ui', size: 50 },
  ];
}

function plot(el, title, opts, data, extra) {
  const div = document.createElement('div');
  div.className = extra?.fullWidth ? 'chart chart-full' : 'chart';
  div.innerHTML = `<h2>${title}</h2>`;
  el.appendChild(div);
  const pad = parseFloat(getComputedStyle(div).paddingLeft) + parseFloat(getComputedStyle(div).paddingRight);
  const w = div.clientWidth ? div.clientWidth - pad : (el.clientWidth ? Math.floor((el.clientWidth - 32) / 2) : 600);
  new uPlot({
    width: w, height: extra?.height || 240,
    series: modernSeries(opts.series),
    axes: opts.axes || modernAxes(),
    scales: opts.scales || {},
    cursor: { drag: { x: true, y: false } },
  }, data, div);
}

async function renderReport(d) {
  const el = $('#report-body');
  el.innerHTML = `<h2>Run ${d.id} — ${d.status}</h2>
    <div class="card"><code class="muted">${esc(d.curl)}</code></div>
    <div class="card">${verdictHTML(d)}</div>` + summaryTableHTML(d);

  const grid = document.createElement('div');
  grid.className = 'chart-grid';
  el.appendChild(grid);

  // Cross-stage charts: x = concurrency, y = latency or throughput.
  // uPlot requires x-values to be sorted ascending and deduplicated. When
  // the user's plan has non-monotonic concurrency (e.g. [5, 10, 20, 1, 1]),
  // we must sort by concurrency and merge duplicates (keeping the entry
  // with the most requests — the heaviest-load data point at that level).
  // The x-axis range is pinned to [0, maxPlanConcurrency] so the chart
  // always reflects the user's full plan, even for aborted runs.
  const maxPlanConc = Math.max(...(d.plan || d.summaries).map(s => s.concurrency));
  const maxPlanReq  = Math.max(...(d.plan || d.summaries).map(s => s.requests));
  const byConc = [...d.summaries].sort((a, b) => a.concurrency - b.concurrency);
  const deduped = [];
  for (const s of byConc) {
    const prev = deduped[deduped.length - 1];
    if (prev && prev.concurrency === s.concurrency) {
      if (s.requests > prev.requests) deduped[deduped.length - 1] = s;
    } else {
      deduped.push(s);
    }
  }

  const CONC_SCALE = { scales: { x: { time: false, range: [0, maxPlanConc] } } };
  const REQ_SCALE  = { scales: { x: { time: false, range: [0, maxPlanReq]  } } };
  if (deduped.length > 1) {
    plot(grid, 'Latency vs concurrency', {
      series: [{ label: 'concurrency' },
        { label: 'p50', value: (u, v) => v != null ? v.toFixed(1) + ' ms' : '—' },
        { label: 'p99', value: (u, v) => v != null ? v.toFixed(1) + ' ms' : '—' }],
      ...CONC_SCALE,
    }, [deduped.map(s => s.concurrency),
        deduped.map(s => s.p50Us / 1000),
        deduped.map(s => s.p99Us / 1000)]);
    plot(grid, 'Throughput vs concurrency', {
      series: [{ label: 'concurrency' },
        { label: 'req/s', value: (u, v) => v != null ? v.toFixed(0) + ' req/s' : '—' }],
      ...CONC_SCALE,
    }, [deduped.map(s => s.concurrency), deduped.map(s => s.rps)]);
  }

  const byReq = [...d.summaries].sort((a, b) => a.requests - b.requests);
  const dedupReq = [];
  for (const s of byReq) {
    const prev = dedupReq[dedupReq.length - 1];
    if (prev && prev.requests === s.requests) {
      if (s.concurrency > prev.concurrency) dedupReq[dedupReq.length - 1] = s;
    } else {
      dedupReq.push(s);
    }
  }
  if (dedupReq.length > 1) {
    plot(grid, 'Latency vs requests', {
      series: [{ label: 'requests' },
        { label: 'p50', value: (u, v) => v != null ? v.toFixed(1) + ' ms' : '—' },
        { label: 'p99', value: (u, v) => v != null ? v.toFixed(1) + ' ms' : '—' }],
      ...REQ_SCALE,
    }, [dedupReq.map(s => s.requests),
        dedupReq.map(s => s.p50Us / 1000),
        dedupReq.map(s => s.p99Us / 1000)]);
    plot(grid, 'Throughput vs requests', {
      series: [{ label: 'requests' },
        { label: 'req/s', value: (u, v) => v != null ? v.toFixed(0) + ' req/s' : '—' }],
      ...REQ_SCALE,
    }, [dedupReq.map(s => s.requests), dedupReq.map(s => s.rps)]);
  }

  // === Unified cross-stage charts ===
  const stageCount = d.summaries.length;
  const seriesPromises = d.summaries.map(s =>
    fetch(`/api/runs/${d.id}/series?stage=${s.stage}`).then(r => r.json()).catch(() => [])
  );
  const errorPromises = d.summaries.map(s =>
    fetch(`/api/runs/${d.id}/errors?stage=${s.stage}`).then(r => r.json()).catch(() => [])
  );
  const [allSeries, allErrors] = await Promise.all([
    Promise.all(seriesPromises),
    Promise.all(errorPromises),
  ]);

  // Chart 1: Combined latency over time (wall-clock timeline, all stages)
  {
    const cc = chartColors();
    const palette = [[cc.c2, cc.c2f], [cc.c1, cc.c1f], [cc.c3, cc.c3f], [cc.c4, cc.c4f]];
    const combinedX = [];
    const stageY = d.summaries.map(() => []);

    for (let si = 0; si < stageCount; si++) {
      const pts = allSeries[si];
      const s = d.summaries[si];
      if (!pts || !pts.length) continue;
      for (const p of pts) {
        combinedX.push((s.startedAtMs + p.offsetMs) / 1000);
        for (let sj = 0; sj < stageCount; sj++)
          stageY[sj].push(sj === si ? p.avgUs / 1000 : null);
      }
    }

    if (combinedX.length) {
      const seriesDefs = [{ label: 'time' }];
      for (let si = 0; si < stageCount; si++) {
        const [stroke, fill] = palette[si % palette.length];
        seriesDefs.push({
          label: `Stage ${d.summaries[si].stage + 1}`,
          stroke, fill: gradientFill(fill), width: 2,
          value: (u, v) => v != null ? v.toFixed(1) + ' ms' : '—',
          points: { show: false },
        });
      }
      plot(grid, 'Latency over time — all stages', { series: seriesDefs },
        [combinedX, ...stageY]);
    }
  }

  // Chart 2: Combined error timeline (wall-clock, skip if zero errors everywhere)
  {
    const totalErrors = allErrors.reduce((sum, pts) =>
      sum + (pts || []).reduce((s, p) => s + p.errors, 0), 0);

    if (totalErrors > 0) {
      const cc = chartColors();
      const palette = [[cc.c4, cc.c4f], [cc.c3, cc.c3f], [cc.c1, cc.c1f], [cc.c2, cc.c2f]];
      const combinedX = [];
      const stageY = d.summaries.map(() => []);

      for (let si = 0; si < stageCount; si++) {
        const pts = allErrors[si];
        const s = d.summaries[si];
        if (!pts || !pts.length) continue;
        for (const p of pts) {
          combinedX.push((s.startedAtMs + p.offsetMs) / 1000);
          for (let sj = 0; sj < stageCount; sj++)
            stageY[sj].push(sj === si ? p.errors : null);
        }
      }

      if (combinedX.length) {
        const seriesDefs = [{ label: 'time' }];
        for (let si = 0; si < stageCount; si++) {
          const [stroke, fill] = palette[si % palette.length];
          seriesDefs.push({
            label: `Stage ${d.summaries[si].stage + 1}`,
            stroke, fill: gradientFill(fill), width: 2,
            value: (u, v) => v != null ? v.toFixed(0) + ' errors' : '—',
            points: { show: false },
          });
        }
        plot(grid, 'Error timeline — all stages', {
          series: seriesDefs,
          scales: { y: { min: 0 } },
        }, [combinedX, ...stageY]);
      }
    }
  }

  // Chart 3: Percentile comparison across stages (full-width nested bar chart)
  // Draws p99 widest in back, then p95, p90, p50 narrowest in front.
  if (stageCount > 0) {
    const cc = chartColors();
    const barFn = (sz) => uPlot.paths.bars({ size: [sz], radius: 2 });
    const valFmt = (u, v) => v != null ? v.toFixed(1) + ' ms' : '—';
    const mAxes = modernAxes();

    const pDiv = document.createElement('div');
    pDiv.className = 'chart chart-full';
    pDiv.innerHTML = '<h2>Percentile comparison across stages</h2>';
    grid.appendChild(pDiv);
    const pPad = parseFloat(getComputedStyle(pDiv).paddingLeft) + parseFloat(getComputedStyle(pDiv).paddingRight);
    const pW = pDiv.clientWidth - pPad;

    new uPlot({
      width: pW, height: 280,
      series: [
        {},
        { label: 'p99', stroke: cc.bar4, fill: cc.bar4, width: 0, paths: barFn(0.65), points: { show: false }, value: valFmt },
        { label: 'p95', stroke: cc.bar3, fill: cc.bar3, width: 0, paths: barFn(0.50), points: { show: false }, value: valFmt },
        { label: 'p90', stroke: cc.bar1, fill: cc.bar1, width: 0, paths: barFn(0.35), points: { show: false }, value: valFmt },
        { label: 'p50', stroke: cc.bar2, fill: cc.bar2, width: 0, paths: barFn(0.20), points: { show: false }, value: valFmt },
      ],
      scales: { x: { time: false, range: [0.3, stageCount + 0.7] } },
      axes: [
        { ...mAxes[0],
          splits: () => Array.from({ length: stageCount }, (_, i) => i + 1),
          values: (u, vals) => vals.map(v => `Stage ${v}`),
        },
        mAxes[1],
      ],
    }, [
      d.summaries.map((_, i) => i + 1),
      d.summaries.map(s => s.p99Us / 1000),
      d.summaries.map(s => s.p95Us / 1000),
      d.summaries.map(s => s.p90Us / 1000),
      d.summaries.map(s => s.p50Us / 1000),
    ], pDiv);
  }
}
async function loadHistory() {
  const runs = await (await fetch('/api/runs')).json();
  $('#history-table tbody').innerHTML = (runs || []).filter(r => r.status !== 'pending').map(r =>
    `<tr style="cursor:pointer" onclick="loadReport(${r.id})">
     <td>${r.id}</td><td>${r.createdAt}</td><td class="muted">${esc(r.curl.slice(0, 60))}</td><td>${r.status}</td></tr>`).join('');
}
window.apiperfViews = { showRun: loadReport, showHistory: loadHistory };
