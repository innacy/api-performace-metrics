'use strict';
const $ = (s) => document.querySelector(s);
const fmtUs = (us) => us >= 1e6 ? (us / 1e6).toFixed(2) + 's' : us >= 1000 ? (us / 1000).toFixed(1) + 'ms' : us + 'µs';
const ms = (v) => v < 1 ? Math.round(v * 1000) + 'µs' : v < 10 ? v.toFixed(1) + 'ms' : v < 1000 ? Math.round(v) + 'ms' : (v / 1000).toFixed(1) + 's';
const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
const css = (v) => getComputedStyle(document.documentElement).getPropertyValue(v).trim();

const state = { runId: null, external: false, plan: [], summaries: [], probe: null, target: null, report: null };

// Theme toggle: persisted in localStorage, defaults to light.
function applyTheme(theme) {
  document.documentElement.classList.toggle('dark', theme === 'dark');
  document.querySelectorAll('#theme-toggle button').forEach(b =>
    b.classList.toggle('active', b.dataset.theme === theme));
  try { localStorage.setItem('apiperf-theme', theme); } catch {}
  drawStairs();
  if (state.report && !$('#view-report').classList.contains('hidden')) drawCharts(); // SVG colours are baked in
}
document.querySelectorAll('#theme-toggle button').forEach(b =>
  b.onclick = () => applyTheme(b.dataset.theme));

function show(view) {
  for (const v of ['setup', 'live', 'report', 'history']) $('#view-' + v).classList.toggle('hidden', v !== view);
}
// The URL hash is the source of truth for which view is open, so a reload
// (or Back) lands on the same report/history/live run: #run/42, #history.
function route() {
  const m = location.hash.match(/^#run\/(\d+)$/);
  if (m) loadReport(+m[1]);
  else if (location.hash === '#history') { loadHistory(); show('history'); }
  else show('setup');
}
function go(hash) {
  if (location.hash === hash) route(); else location.hash = hash;
}
window.onhashchange = route;
document.querySelectorAll('nav a').forEach(a => a.onclick = () =>
  go(a.dataset.view === 'history' ? '#history' : '#new'));

// redact hides credentials when showing a curl (copy still gives the full one).
const redact = (curl) => curl
  .replace(/(-H\s*['"]?\s*(?:authorization|proxy-authorization|cookie|x-api-key|api-key|x-auth-token)\s*:\s*)([^'"\n]*)/gi,
    (_, h, v) => h + (v.match(/^(\w+\s+)/)?.[1] || '') + '••••••')
  .replace(/((?:\s|^)(?:-u|--user)\s+['"]?)[^'"\s]+/g, '$1••••••');

// ---------- tooltip + small SVG helpers ----------
const tip = $('#tip');
const showTip = (e, html) => { tip.innerHTML = html; tip.style.opacity = 1; tip.style.left = Math.min(e.clientX + 14, innerWidth - 290) + 'px'; tip.style.top = e.clientY + 14 + 'px'; };
const hideTip = () => { tip.style.opacity = 0; };
const logScale = (lo, hi, a, b) => (v) => a + (Math.log(Math.max(v, lo)) - Math.log(lo)) / (Math.log(hi) - Math.log(lo)) * (b - a);
const logTicks = (lo, hi) => {
  const out = [];
  for (let p = Math.floor(Math.log10(lo)); p <= Math.ceil(Math.log10(hi)); p++)
    for (const m of [1, 3]) { const v = m * 10 ** p; if (v >= lo && v <= hi) out.push(v); }
  return out;
};
const polyline = (pts, color, w = 2) => pts.length ? `<polyline fill="none" stroke="${color}" stroke-width="${w}" stroke-linejoin="round" stroke-linecap="round" points="${pts.map(p => p.join(',')).join(' ')}"/>` : '';
const svgX = (svg, e) => { const r = svg.getBoundingClientRect(); return (e.clientX - r.left) / r.width * svg.viewBox.baseVal.width; };
function toggles(el, items, st, redraw) {
  el.innerHTML = items.map(([k, label, color]) => `<button class="tg ${st[k] ? 'on' : ''}" data-k="${k}"><i style="background:var(${color})"></i>${label}</button>`).join('');
  el.onclick = (e) => { const b = e.target.closest('button'); if (!b) return; st[b.dataset.k] = !st[b.dataset.k]; b.classList.toggle('on'); redraw(); };
}
function seg(el, onPick) {
  el.onclick = (e) => { const b = e.target.closest('button'); if (!b) return; el.querySelectorAll('button').forEach(x => x.classList.toggle('on', x === b)); onPick(b.dataset.v); };
}

// ---------- probe timing ----------
const PHASES = [
  ['dnsMs', 'DNS lookup', 'Turning the hostname into an IP address. Slow? Check your resolver, or use an IP.'],
  ['connectMs', 'TCP connect', 'Opening a connection to the server: mostly network distance.'],
  ['tlsMs', 'TLS handshake', 'Agreeing on encryption. Reused connections skip it.'],
  ['sendMs', 'Request sent', 'Uploading headers and body. Large only for big payloads.'],
  ['waitMs', 'Server processing', 'Time to first byte: your code, DB queries, downstream calls. This is where to optimise.'],
  ['recvMs', 'Content download', 'Receiving the body. Large? Paginate or enable compression.'],
];
const phaseTotal = (ph) => PHASES.reduce((s, [k]) => s + ph[k], 0);
const fmtMs = (v) => v < 10 ? v.toFixed(1) : Math.round(v).toString();
const phaseStack = (ph, scale, cls = 'pt-stack') => `<div class="${cls}">` + PHASES.map(([k, name]) =>
  `<div title="${name} ${fmtMs(ph[k])}ms" style="width:${ph[k] / scale * 100}%;background:var(--ph-${k})"></div>`).join('') + `</div>`;
const biggestPhase = (c) => PHASES.reduce((a, b) => c[b[0]] > c[a[0]] ? b : a);

// probeTimingHTML explains where the probe's time went, step by step.
function probeTimingHTML(p) {
  const c = p.cold;
  if (!c) return '';
  const total = phaseTotal(c) || 1;
  const setup = c.dnsMs + c.connectMs + c.tlsMs;
  const biggest = biggestPhase(c);
  let at = 0;
  const rows = PHASES.map(([k, name, why]) => {
    const row = `<div class="pt-lbl"><span class="pt-dot" style="background:var(--ph-${k})"></span>${name}</div>
      <div class="pt-track"><div style="left:${at / total * 100}%;width:${c[k] / total * 100}%;background:var(--ph-${k})"></div></div>
      <div class="pt-ms"><b>${fmtMs(c[k])}</b> ms</div><div class="pt-why">${why}</div>`;
    at += c[k];
    return row;
  }).join('');
  const w = p.warm;
  const compare = w ? `<div class="pt-pair">
      <div><b>First request</b> <span class="muted">new connection, ${fmtMs(total)} ms</span>${phaseStack(c, total)}</div>
      <div><b>Repeat request</b> <span class="muted">${w.reused ? 'reused connection' : 'new connection'}, ${fmtMs(phaseTotal(w))} ms</span>${phaseStack(w, total)}</div>
    </div>
    <p class="muted" style="margin:.3rem 0 0">The load test mostly reuses connections, so its numbers look like the repeat request.</p>` : '';
  return `<p style="margin:.6rem 0"><b>${biggest[1]} took most of the time (${Math.round(c[biggest[0]] / total * 100)}%).</b>
      <span class="muted">Connection setup (DNS + TCP + TLS) took ${fmtMs(setup)} ms. ${p.proto || ''}</span></p>
    ${phaseStack(c, total)}<div class="pt-wf">${rows}</div>${compare}`;
}

// =====================================================================
// New run
// =====================================================================
const PRESETS = {
  smoke:    { name: 'Smoke',     desc: 'Does it work under a little load? Seconds.', plan: [[100, 5]] },
  quick:    { name: 'Quick',     desc: 'Up to 10k requests. About a minute.', plan: [[100, 10], [1000, 25], [10000, 50]] },
  standard: { name: 'Standard',  desc: 'Find the breaking point up to 100 at once.', plan: [[100, 10], [1000, 25], [10000, 50], [50000, 100]] },
  full:     { name: 'Full ramp', desc: 'Up to 1M requests. Long, heavy.', plan: [[100, 10], [1000, 25], [10000, 50], [100000, 100], [1000000, 250]] },
  custom:   { name: 'Custom',    desc: 'Edit the stages yourself.', plan: null },
};
const RULE_PRESETS = { strict: [1, 1000, 5], balanced: [5, 2000, 10], lenient: [50, 5000, 30] };
const MAX_CONC = 10000; // matches the server's maxStageConcurrency
let preset = 'quick';
state.plan = PRESETS.quick.plan.map(([requests, concurrency]) => ({ requests, concurrency }));
const rules = { err: 5, p99: 2000, to: 10 };

// clampInt coerces a raw input value to a finite integer in [lo, hi], so a
// blank or garbage field can't submit a plan that hangs or blows up the engine.
function clampInt(raw, lo, hi) {
  const n = Math.floor(Number(raw));
  return Number.isFinite(n) ? Math.min(Math.max(n, lo), hi) : lo;
}
const fmtN = (n) => n >= 1e6 ? +(n / 1e6).toFixed(1) + 'M' : n >= 1e3 ? +(n / 1e3).toFixed(1) + 'k' : String(n);
const fmtT = (s) => s < 60 ? Math.max(1, Math.round(s)) + ' s' : s < 3600 ? Math.round(s / 60) + ' min' : (s / 3600).toFixed(1) + ' h';
// Latency the estimate is based on: the warm (reused-connection) probe, like the load test.
const probeMs = () => { const p = state.probe; if (!p || p.error) return null; return Math.max(0.2, p.warm ? phaseTotal(p.warm) : p.latencyMs); };
// ponytail: assumes latency stays at probe level; past saturation real runs take longer.
const estSecs = (st) => probeMs() == null ? null : st.requests * probeMs() / 1000 / st.concurrency;
const apiRules = () => ({ maxErrorRate: rules.err / 100, maxP99Ms: rules.p99, timeoutMs: rules.to * 1000 });

function renderPresets() {
  $('#presets').innerHTML = Object.entries(PRESETS).map(([k, p]) =>
    `<button class="preset ${k === preset ? 'on' : ''}" data-k="${k}"><b>${p.name}</b><small>${p.desc}</small></button>`).join('');
}
$('#presets').onclick = (e) => {
  const b = e.target.closest('.preset'); if (!b) return;
  preset = b.dataset.k;
  if (PRESETS[preset].plan) state.plan = PRESETS[preset].plan.map(([requests, concurrency]) => ({ requests, concurrency }));
  renderPlan();
};

function renderPlan() {
  renderPresets();
  $('#plan-table tbody').innerHTML = state.plan.map((s, i) => `<tr>
    <td>${i + 1}</td>
    <td><input type="number" min="1" value="${s.requests}" data-i="${i}" data-f="requests"></td>
    <td><input type="number" min="1" max="${MAX_CONC}" value="${s.concurrency}" data-i="${i}" data-f="concurrency"></td>
    <td class="num"></td>
    <td><button class="x" title="Remove stage" data-rm="${i}" ${state.plan.length < 2 ? 'disabled' : ''}>✕</button></td></tr>`).join('');
  renderEstimates();
}
function renderEstimates() {
  document.querySelectorAll('#plan-table tbody tr').forEach((tr, i) => {
    const t = estSecs(state.plan[i]);
    tr.querySelector('.num').textContent = t == null ? '–' : fmtT(t);
  });
  const total = state.plan.reduce((a, s) => a + s.requests, 0);
  const secs = probeMs() == null ? null : state.plan.reduce((a, s) => a + estSecs(s), 0) + 3 * (state.plan.length - 1);
  $('#e-total').textContent = total.toLocaleString();
  $('#e-time').textContent = secs == null ? '–' : '~' + fmtT(secs);
  $('#e-time-note').textContent = secs == null ? 'verify to estimate' : `from the ${fmtMs(probeMs())} ms probe; real time varies`;
  $('#e-disk').textContent = '~' + Math.max(1, Math.round(total * 40 / 1e6)) + ' MB';
  drawStairs();
  renderReady();
}
$('#plan-table').oninput = (e) => {
  const i = e.target.dataset.i; if (i == null) return;
  const f = e.target.dataset.f;
  state.plan[i][f] = clampInt(e.target.value, 1, f === 'concurrency' ? MAX_CONC : 1e9);
  preset = 'custom'; renderPresets(); renderEstimates();
};
$('#plan-table').onclick = (e) => {
  const i = e.target.dataset.rm; if (i == null || state.plan.length < 2) return;
  state.plan.splice(+i, 1); preset = 'custom'; renderPlan();
};
$('#btn-add').onclick = () => {
  const l = state.plan[state.plan.length - 1];
  state.plan.push({ requests: l.requests * 2, concurrency: Math.min(l.concurrency * 2, MAX_CONC) });
  preset = 'custom'; renderPlan();
};
$('#btn-ramp').onclick = () => $('#ramp').classList.toggle('hidden');
$('#r-go').onclick = () => {
  const a = clampInt($('#r-from').value, 1, MAX_CONC), b = clampInt($('#r-to').value, 1, MAX_CONC);
  const k = clampInt($('#r-n').value, 1, 20), n = clampInt($('#r-req').value, 1, 1e9);
  // Geometric steps: each stage multiplies concurrency by the same factor.
  state.plan = Array.from({ length: k }, (_, i) => ({ requests: n, concurrency: Math.round(k === 1 ? b : a * Math.pow(b / a, i / (k - 1))) }));
  preset = 'custom'; renderPlan();
};

// Staircase: width = estimated time share (min sliver so short stages stay visible), height = concurrency.
function drawStairs() {
  const plan = state.plan, L = 40, R = 890, T = 14, B = 96, maxC = Math.max(...plan.map(s => s.concurrency));
  const raw = plan.map(s => probeMs() == null ? 1 : estSecs(s)), rawSum = raw.reduce((a, b) => a + b, 0);
  const share = raw.map(v => Math.max(v / rawSum, .06)), shareSum = share.reduce((a, b) => a + b, 0);
  let x = L, g = `<text x="${L - 6}" y="${T + 4}" text-anchor="end">${maxC}</text><text x="${L - 6}" y="${B}" text-anchor="end">0</text>`;
  plan.forEach((s, i) => {
    const ww = share[i] / shareSum * (R - L), h = s.concurrency / maxC * (B - T);
    g += `<rect x="${x + 1}" y="${B - h}" width="${Math.max(ww - 2, 1)}" height="${h}" rx="4" fill="${css('--accent')}" opacity="${.35 + .65 * (i + 1) / plan.length}"/>`;
    if (ww > 50) g += `<text x="${x + ww / 2}" y="${B + 16}" text-anchor="middle">${fmtN(s.requests)} @ ${s.concurrency}</text>`;
    x += ww;
  });
  $('#stairs').innerHTML = g + `<text x="${R}" y="${T}" text-anchor="end">requests at once, over ${probeMs() == null ? 'stages' : 'time'} →</text>`;
}

// Abort rules: slider and number box stay in sync.
const fmtRule = { err: (v) => v + '%', p99: (v) => v >= 1000 ? v / 1000 + ' s' : v + ' ms', to: (v) => v + ' s' };
const RULE_LIMITS = { err: [0, 100], p99: [1, 600000], to: [1, 120] };
function syncRules() {
  for (const k in rules) { $('#s-' + k).value = rules[k]; $('#n-' + k).value = rules[k]; $('#v-' + k).textContent = fmtRule[k](rules[k]); }
  document.querySelectorAll('#rule-presets button').forEach(b =>
    b.classList.toggle('on', RULE_PRESETS[b.dataset.v].join() === [rules.err, rules.p99, rules.to].join()));
  renderReady();
}
for (const k in rules) for (const p of ['s', 'n']) $(`#${p}-${k}`).onchange = $(`#${p}-${k}`).oninput = (e) => {
  if (e.target.value === '') return; // let the user clear the box while typing
  rules[k] = clampInt(e.target.value, ...RULE_LIMITS[k]);
  if (e.type === 'change' || p === 's') syncRules(); else { $('#s-' + k).value = rules[k]; $('#v-' + k).textContent = fmtRule[k](rules[k]); renderReady(); }
};
$('#rule-presets').onclick = (e) => {
  const b = e.target.closest('button'); if (!b) return;
  [rules.err, rules.p99, rules.to] = RULE_PRESETS[b.dataset.v];
  syncRules();
};

function renderReady() {
  const total = state.plan.reduce((a, s) => a + s.requests, 0), maxC = Math.max(...state.plan.map(s => s.concurrency));
  $('#sentence').innerHTML = `We'll send <b>${total.toLocaleString()} requests</b> in <b>${state.plan.length} stage${state.plan.length > 1 ? 's' : ''}</b>, up to <b>${maxC} at once</b>,
    and stop early if more than <b>${rules.err}%</b> fail or the slowest 1% exceed <b>${fmtRule.p99(rules.p99)}</b>.`;
  const verified = state.probe && !state.probe.error && $('#curl').value === state.verifiedCurl;
  const permitted = !state.external || $('#confirm-external').checked;
  $('#btn-start').disabled = !verified || !permitted;
  $('#start-hint').textContent = !verified ? 'Verify the curl first.' : !permitted ? 'Tick the permission box to start.' : 'You can stop the test at any time.';
}
$('#confirm-external').onchange = renderReady;
$('#curl').oninput = renderReady;

// requestChips summarises the parsed curl so the user sees what we understood.
function requestChips(t) {
  let host = '', path = '';
  // curlparse.Target has no JSON tags, so its keys are the Go field names.
  try { const u = new URL(t.URL); host = u.host; path = u.pathname + (u.search ? '?…' : ''); } catch {}
  const headers = Object.keys(t.Header || {});
  const auth = headers.filter(h => /^(authorization|cookie|x-api-key|api-key|x-auth-token)$/i.test(h)).length;
  const body = t.Body ? atob(t.Body).length : 0;
  const type = (t.Header?.['Content-Type'] || [])[0] || '';
  return `<span class="rchip"><b>${esc(t.Method)}</b></span><span class="rchip">host <b>${esc(host)}</b></span><span class="rchip">path <b>${esc(path)}</b></span>
    <span class="rchip"><b>${headers.length}</b> header${headers.length === 1 ? '' : 's'}${auth ? ` (${auth} auth)` : ''}</span>
    ${body ? `<span class="rchip">${/json/i.test(type) ? 'JSON ' : ''}body <b>${body.toLocaleString()} B</b></span>` : ''}
    ${t.Insecure ? '<span class="rchip">TLS check <b>off (-k)</b></span>' : ''}${t.FollowRedirects ? '<span class="rchip">follows redirects</span>' : ''}`;
}

function renderProbe(p) {
  $('#probe-detail').classList.add('hidden');
  if (p.error) {
    $('#probe-line').innerHTML = `<span class="bad">✗ Probe failed: ${esc(p.error)}</span><span class="muted">Fix the target before starting.</span>`;
    return;
  }
  const c = p.cold;
  $('#probe-line').innerHTML = `<b class="${p.status < 400 ? 'good' : 'bad'}">${p.status < 400 ? '✓' : '✗'} HTTP ${p.status}</b><span>${p.latencyMs} ms</span>
    <span class="muted">${p.bytes.toLocaleString()} bytes${p.proto ? ' · ' + esc(p.proto) : ''}</span>` +
    (c ? `${phaseStack(c, phaseTotal(c) || 1, 'mini')}<span class="muted">mostly ${biggestPhase(c)[1].toLowerCase()}</span>
    <button class="link" id="btn-probe-detail">step by step ▾</button>` : '');
  $('#probe-detail').innerHTML = probeTimingHTML(p) +
    (p.snippet ? `<pre class="muted" style="white-space:pre-wrap;margin:.6rem 0 0">${esc(p.snippet.slice(0, 300))}</pre>` : '');
  const btn = $('#btn-probe-detail');
  if (btn) btn.onclick = () => {
    const open = $('#probe-detail').classList.toggle('hidden');
    btn.textContent = open ? 'step by step ▾' : 'hide steps ▴';
  };
}

// createRun posts what's on screen; the server probes the target once and
// returns a pending run. It evicts any superseded pending entry for this curl.
async function createRun() {
  const curl = $('#curl').value;
  const res = await fetch('/api/runs', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ curl, stages: state.plan, rules: apiRules() }),
  });
  const body = await res.json();
  if (!res.ok) throw new Error(body.error || 'failed to create run');
  state.runId = body.runId;
  state.external = body.external;
  state.probe = body.probe;
  state.target = body.target;
  // Snapshot what this run was created with. Start reuses it as-is unless
  // something is edited, avoiding a second probe / orphaned run per test.
  state.verifiedCurl = curl;
  state.verifiedConfig = JSON.stringify([state.plan, apiRules()]);
  return body;
}

$('#btn-verify').onclick = async () => {
  $('#verify-err').textContent = '';
  $('#btn-verify').disabled = true;
  try {
    const body = await createRun();
    $('#req-chips').innerHTML = requestChips(body.target);
    $('#probe-line').classList.remove('hidden');
    renderProbe(body.probe);
    $('#warn-host').textContent = (() => { try { return new URL(body.target.URL).host; } catch { return 'This host'; } })();
    $('#warn-external').classList.toggle('hidden', !body.external);
    $('#confirm-external').checked = false;
    renderEstimates();
  } catch (e) {
    $('#verify-err').textContent = e.message === 'Failed to fetch' ? 'Cannot reach the apiperf server — is it running?' : e.message;
  } finally {
    $('#btn-verify').disabled = false;
  }
};

$('#btn-start').onclick = async () => {
  const unchanged = state.runId != null && $('#curl').value === state.verifiedCurl &&
    JSON.stringify([state.plan, apiRules()]) === state.verifiedConfig;
  const startRun = () => fetch(`/api/runs/${state.runId}/start`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ confirmExternal: $('#confirm-external').checked }),
  });
  $('#btn-start').disabled = true;
  try {
    if (!unchanged) await createRun(); // plan/rules edited since Verify
    state.summaries = [];
    // Connect SSE and wait for it to open (or 2s) before starting, so the
    // first stage_start isn't lost to the EventSource handshake.
    initLiveTable(state.plan);
    await connectSSE();
    show('live');
    let start = await startRun();
    if (start.status === 409) {
      // The run we tried was already started/finished (stale snapshot):
      // mint a fresh run for what's on screen and retry once.
      await createRun();
      start = await startRun();
    }
    if (!start.ok) throw new Error((await start.json()).error);
    history.replaceState(null, '', `#run/${state.runId}`); // reload reattaches; no hashchange fired
  } catch (e) {
    if (es) es.close();
    $('#verify-err').textContent = e.message;
    show('setup');
    renderReady();
  }
};

// =====================================================================
// Live run
// =====================================================================
$('#btn-stop').onclick = async () => {
  if (!confirm('Stop this run? Results so far are kept.')) return;
  $('#btn-stop').disabled = true;
  $('#live-status').textContent = 'Stopping…';
  const res = await fetch(`/api/runs/${state.runId}/stop`, { method: 'POST' });
  if (!res.ok) { $('#live-status').textContent = (await res.json()).error; $('#btn-stop').disabled = false; }
};

function initLiveTable(plan) {
  $('#live-table tbody').innerHTML = plan.map((s, i) =>
    `<tr id="stage-${i}"><td>${i + 1}</td><td>${s.requests.toLocaleString()}</td><td>${s.concurrency}</td>
     <td class="c-prog">–</td><td class="c-rps">–</td><td class="c-p50">–</td><td class="c-p90">–</td>
     <td class="c-p95">–</td><td class="c-p99">–</td><td class="c-err">–</td><td class="c-status muted">waiting</td></tr>`).join('');
  $('#live-status').textContent = '';
  $('#btn-stop').disabled = false;
}

function fillDoneRow(d) {
  const row = $(`#stage-${d.stage}`);
  if (!row) return;
  const fr = failRate(d);
  row.querySelector('.c-prog').textContent = '100%';
  row.querySelector('.c-rps').textContent = d.rps.toFixed(0);
  row.querySelector('.c-p50').textContent = fmtUs(d.p50Us);
  row.querySelector('.c-p90').textContent = fmtUs(d.p90Us);
  row.querySelector('.c-p95').textContent = fmtUs(d.p95Us);
  row.querySelector('.c-p99').textContent = fmtUs(d.p99Us);
  row.querySelector('.c-err').innerHTML = fr ? `<span class="bad">${pct(fr)}</span>` : '<span class="good">0</span>';
  row.querySelector('.c-status').textContent = 'done';
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
      if (row) row.querySelector('.c-status').textContent = 'running';
    } else if (ev.type === 'progress') {
      const row = $(`#stage-${d.stage}`);
      if (!row) return;
      row.querySelector('.c-prog').textContent = `${Math.round(100 * d.done / d.total)}%`;
      row.querySelector('.c-rps').textContent = d.rps.toFixed(0);
      row.querySelector('.c-p99').textContent = d.p99Ms.toFixed(1) + 'ms';
    } else if (ev.type === 'stage_done') {
      state.summaries.push(d);
      fillDoneRow(d);
    } else if (ev.type === 'run_done') {
      es.close();
      const finishedId = state.runId;
      $('#live-status').textContent = `Run ${d.status}.` + (d.verdict && d.verdict.abortReason ? ` Reason: ${d.verdict.abortReason}` : '');
      // This run is finished: the next Start must mint a fresh run.
      state.runId = null; state.verifiedCurl = null; state.verifiedConfig = null;
      go(`#run/${finishedId}`);
    }
  };
  return new Promise((resolve) => {
    es.onopen = resolve;
    setTimeout(resolve, 2000);
  });
}

// =====================================================================
// Report
// =====================================================================
async function loadReport(id) {
  const res = await fetch(`/api/runs/${id}`);
  if (!res.ok) { $('#report-body').innerHTML = `<p class="bad">Run ${id} not found.</p>`; show('report'); return; }
  const d = await res.json();
  if (d.status === 'running') {
    // Reattach to a live run (e.g. after a page reload).
    state.runId = d.id; state.plan = d.plan; state.summaries = d.summaries || [];
    initLiveTable(d.plan);
    state.summaries.forEach(fillDoneRow);
    show('live');
    connectSSE();
    return;
  }
  d.summaries = d.summaries || [];
  await renderReport(d);
  show('report');
  drawCharts();
}

// Failures include 5xx responses, not just transport errors/timeouts.
// Older runs lack failCount, so derive it from the codes.
const served = (s) => Object.values(s.codes || {}).reduce((a, n) => a + n, 0) + s.errorCount;
const fails = (s) => s.failCount ?? s.errorCount + Object.entries(s.codes || {}).reduce((a, [c, n]) => a + (+c >= 500 ? n : 0), 0);
const failRate = (s) => served(s) ? fails(s) / served(s) : 0;
const pct = (x) => (x * 100).toFixed(x > 0 && x < 0.001 ? 2 : 1) + '%';
const stageHealthy = (d, s) => {
  const sat = d.verdict?.saturationStage;
  return !(sat != null && s.stage >= sat) && failRate(s) <= 0.01;
};
const codeChips = (s) => Object.entries(s.codes || {}).map(([c, n]) =>
  `<span class="chip ${+c >= 400 ? 'e' : ''}">${c} ×${n.toLocaleString()}</span>`).join('') +
  (s.errorCount ? `<span class="chip e">error ×${s.errorCount.toLocaleString()}</span>` : '');

// plainWords turns the verdict into short sentences anyone can read.
function plainWords(d) {
  const v = d.verdict || {}, S = d.summaries, out = [];
  if (!S.length) return ['No stage completed.'];
  const peak = S[S.length - 1], sat = v.saturationStage;
  if (sat == null) {
    out.push(`Up to <b>${peak.concurrency} requests at once</b>, no slowdown detected (slowest 1% under ${fmtUs(peak.p99Us)}).`);
  } else {
    if (sat > 0) out.push(`Up to <b>${S[sat - 1].concurrency} requests at once</b>, your API stays fast (slowest 1% under ${fmtUs(S[sat - 1].p99Us)}).`);
    const s = S[sat], base = S[0].p99Us, why = [];
    if (base && s.p99Us >= 4 * base) why.push(`the slowest requests get ${Math.round(s.p99Us / base)}× slower`);
    if (fails(s)) why.push(`<b class="bad">${pct(failRate(s))} of requests fail</b>`);
    out.push(`At <b>${s.concurrency} at once</b>, ${why.join(' and ') || 'it starts to degrade'}: this is the breaking point.`);
  }
  const fiveXX = S.reduce((a, s) => a + fails(s) - s.errorCount, 0);
  if (sat == null && fiveXX) out.push(`<span class="bad">${fiveXX.toLocaleString()} responses were server errors (5xx).</span> Check the server logs.`);
  if (v.generatorLimited) out.push('Throughput stopped growing while the API stayed healthy, so this machine may be the limit, not your API.');
  if (v.abortReason) out.push(`<span class="bad">Stopped early: ${esc(v.abortReason)}.</span>`);
  if (d.status === 'killed') out.push('Stopped by you before all stages finished.');
  for (const n of v.notes || []) if (n.startsWith('warning')) out.push(`<span class="bad">${esc(n)}</span>`);
  return out;
}

// delta shows the change vs the previous run of the same curl (lower is better).
function delta(cur, prev, prevId) {
  if (!prev) return '';
  const r = cur / prev - 1;
  if (Math.abs(r) < 0.02) return `<small class="muted">≈ same as run ${prevId}</small>`;
  return `<small class="${r > 0 ? 'bad' : 'good'}">${r > 0 ? '▲' : '▼'} ${Math.round(Math.abs(r) * 100)}% vs run ${prevId}</small>`;
}

function kpisHTML(d, prev) {
  const v = d.verdict || {}, S = d.summaries, peak = S[S.length - 1];
  const capStage = v.saturationStage != null ? v.saturationStage : S.length;
  const total = S.reduce((a, s) => a + served(s), 0), failed = S.reduce((a, s) => a + fails(s), 0);
  const pp = prev?.peak;
  return `<div class="kpis">
    <div class="kpi"><small>Comfortable capacity</small><b>${v.maxHealthyRps ? Math.round(v.maxHealthyRps).toLocaleString() + ' req/s' : '—'}</b>
      <small>${v.maxHealthyRps ? `up to stage ${capStage}` : 'slow from the first stage'}</small></div>
    <div class="kpi"><small>Typical response (p50)</small><b>${fmtUs(peak.p50Us)}</b>${delta(peak.p50Us, pp?.p50Us, prev?.id) || `<small>at peak, stage ${peak.stage + 1}</small>`}</div>
    <div class="kpi"><small>Slowest 1% (p99)</small><b>${fmtUs(peak.p99Us)}</b>${delta(peak.p99Us, pp?.p99Us, prev?.id) || `<small>at peak, stage ${peak.stage + 1}</small>`}</div>
    <div class="kpi"><small>Failed requests</small><b class="${failed ? 'bad' : 'good'}">${total ? pct(failed / total) : '—'}</b>
      <small>${failed.toLocaleString()} of ${total.toLocaleString()}, incl. 5xx</small></div>
  </div>`;
}

function stagesTableHTML(d) {
  return `<div class="card tblwrap"><table><thead><tr><th>Stage</th><th>Requests</th><th>At once</th><th>RPS</th><th>p50</th><th>p90</th><th>p95</th><th>p99</th><th>max</th><th class="chips">Responses</th><th>Health</th></tr></thead><tbody>` +
    d.summaries.map(s => `<tr><td>${s.stage + 1}</td><td>${s.requests.toLocaleString()}</td><td>${s.concurrency}</td>
      <td>${s.rps.toFixed(0)}</td><td>${fmtUs(s.p50Us)}</td><td>${fmtUs(s.p90Us)}</td><td>${fmtUs(s.p95Us)}</td>
      <td>${fmtUs(s.p99Us)}</td><td>${fmtUs(s.maxUs)}</td><td class="chips">${codeChips(s)}</td>
      <td>${stageHealthy(d, s) ? '<span class="good">● healthy</span>' : '<span class="bad">● degraded</span>'}</td></tr>`).join('') +
    `</tbody></table></div>`;
}

const chartCard = (id, title, desc, ctrls, look, extra = '') => `<div class="card">
  <div class="chart-head"><div><h3>${title}</h3><p>${desc}</p></div><div class="ctrls">${ctrls}</div></div>
  <svg class="ch" id="${id}"></svg>${extra}${look ? `<div class="look">What to look for: ${look}</div>` : ''}</div>`;

const getJSON = (url) => fetch(url).then(r => r.ok ? r.json() : null).catch(() => null);

async function renderReport(d) {
  const el = $('#report-body');
  const S = d.summaries;
  // Same-curl runs before this one, newest first: candidates for "compare".
  const runs = (await getJSON('/api/runs')) || [];
  const earlier = runs.filter(r => r.id < d.id && r.curl === d.curl && r.peak && r.status !== 'running');
  const series = S.length ? await Promise.all(S.map(s => getJSON(`/api/runs/${d.id}/series?stage=${s.stage}`).then(p => p || []))) : [];
  state.report = { d, series, earlier, hist: {}, compare: {}, prevId: earlier[0]?.id ?? null,
    A: { rps: true, p50: true, p95: false, p99: true }, B: { p50: true, p95: false, p99: true, errs: true, scale: 'equal' },
    C: { stage: Math.max(0, S.length - 1), mode: 'hist' } };

  const probe = d.probe && d.probe.cold ? `<h2>One request, step by step (from Verify)</h2><div class="card">
      <div><b class="${d.probe.status < 400 ? 'good' : 'bad'}">HTTP ${d.probe.status}</b> · ${d.probe.latencyMs} ms · ${d.probe.bytes.toLocaleString()} bytes</div>
      ${probeTimingHTML(d.probe)}</div>` : '';
  const stageSeg = S.map((s, i) => `<button data-v="${i}" class="${i === state.report.C.stage ? 'on' : ''}">S${s.stage + 1}</button>`).join('');
  const charts = S.length ? `<h2>Charts</h2>` +
    chartCard('ch-a', 'Where does it break?', 'Throughput (bars) and response times (lines) for each step of load. The red zone is where it got slower or started failing.',
      `<span class="ctrls" id="ch-a-tg"></span>`, '<b>bars stop growing while lines shoot up</b> = the API is saturated. More load only adds waiting.') +
    chartCard('ch-b', 'What happened over time', 'Response times through the whole run. Red ticks at the bottom are failed requests.',
      `<span class="ctrls" id="ch-b-tg"></span><span class="seg" id="ch-b-scale"><button class="on" data-v="equal">Equal per stage</button><button data-v="real">Real time</button></span>`,
      '<b>spikes inside a stage</b> (GC pauses, cold caches) and <b>a slow upward drift</b> (leaks, queue build-up).') +
    chartCard('ch-c', 'How fast were most requests?', 'Hover anywhere to read “<b>X%</b> of requests finished within <b>Y</b>”.',
      `<span class="seg" id="ch-c-stage">${stageSeg}</span><span class="seg" id="ch-c-mode"><button class="on" data-v="hist">Histogram</button><button data-v="cdf">Cumulative</button></span>`,
      '<b>two humps</b> = two kinds of requests (cache hit vs miss). <b>A long tail</b> right of p99 = some users wait much longer.') +
    chartCard('ch-d', 'Which responses came back?', 'Share of each response type over time. A healthy run stays green.', '', '',
      `<div class="legend"><span><i style="background:var(--good)"></i>2xx/3xx success</span><span><i style="background:var(--c4xx)"></i>4xx client error</span><span><i style="background:var(--bad)"></i>5xx server error</span><span><i style="background:var(--cerr)"></i>timeout / connection error</span></div>`) +
    (earlier.length
      ? chartCard('ch-e', 'Better or worse than last time?', 'Typical (p50) to slowest-1% (p99) range per stage, this run vs an earlier run of the same curl.',
        `<span class="seg" id="ch-e-pick">${earlier.slice(0, 4).map((r, i) => `<button data-v="${r.id}" class="${i ? '' : 'on'}">vs run ${r.id}</button>`).join('')}</span>`, '',
        `<div class="legend"><span><i style="background:var(--prev)"></i>earlier run</span><span><i style="background:var(--accent)"></i>this run</span></div>`)
      : `<div class="card muted">Run this curl again to compare runs side by side.</div>`) : '';

  el.innerHTML = `<h2>Run ${d.id} · <span class="status s-${d.status}">${d.status}</span> · ${new Date(d.createdAt).toLocaleString()}</h2>
    <div class="card" style="position:relative">
      <button class="copy-curl-btn" onclick="copyReportCurl(this)" data-curl="${esc(d.curl)}" title="Copy curl">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>
      </button>
      <code class="muted">${esc(redact(d.curl))}</code>
    </div>` +
    (S.length ? kpisHTML(d, earlier[0]) : '') +
    `<div class="card"><b>In plain words</b><ul class="plain">${plainWords(d).map(s => `<li>${s}</li>`).join('')}</ul></div>` +
    probe + (S.length ? `<h2>Stages</h2>` + stagesTableHTML(d) : '') + charts;

  if (!S.length) return;
  const R = state.report;
  toggles($('#ch-a-tg'), [['rps', 'Throughput', '--rps'], ['p50', 'p50', '--p50'], ['p95', 'p95', '--p95'], ['p99', 'p99', '--p99']], R.A, drawA);
  toggles($('#ch-b-tg'), [['p50', 'p50', '--p50'], ['p95', 'p95', '--p95'], ['p99', 'p99', '--p99'], ['errs', 'Failures', '--bad']], R.B, drawB);
  seg($('#ch-b-scale'), (v) => { R.B.scale = v; drawB(); });
  seg($('#ch-c-stage'), (v) => { R.C.stage = +v; drawC(); });
  seg($('#ch-c-mode'), (v) => { R.C.mode = v; drawC(); });
  if (earlier.length) seg($('#ch-e-pick'), (v) => { R.prevId = +v; drawE(); });
}

function copyReportCurl(btn) {
  navigator.clipboard.writeText(btn.dataset.curl).then(() => {
    btn.classList.add('copied');
    setTimeout(() => btn.classList.remove('copied'), 1500);
  });
}

function drawCharts() {
  if (!state.report || !state.report.d.summaries.length) return;
  drawA(); drawB(); drawC(); drawD(); drawE();
}

// ---------- A: where does it break ----------
function drawA() {
  const { d, A } = state.report, S = d.summaries, svg = $('#ch-a');
  const W = 760, H = 300, L = 52, R = 700, T = 18, B = 258, bw = (R - L) / S.length;
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  const sat = d.verdict?.saturationStage, abortMs = d.rules?.maxP99Ms;
  const lats = S.flatMap(s => [s.p50Us, s.p95Us, s.p99Us].map(u => u / 1000));
  const lo = Math.max(0.01, Math.min(...lats) / 2), hi = Math.max(...lats, abortMs && abortMs < Math.max(...lats) * 3 ? abortMs : 0) * 1.6;
  const yL = logScale(lo, hi, B, T), maxR = Math.max(...S.map(s => s.rps)) * 1.15 || 1, yR = (v) => B - v / maxR * (B - T);
  let g = '';
  for (const v of logTicks(lo, hi)) g += `<line x1="${L}" x2="${R}" y1="${yL(v)}" y2="${yL(v)}" stroke="${css('--grid')}"/><text x="${R + 6}" y="${yL(v) + 4}">${ms(v)}</text>`;
  for (let i = 0; i <= 4; i++) { const v = maxR / 1.15 * i / 4; g += `<text x="${L - 6}" y="${yR(v) + 4}" text-anchor="end">${fmtN(Math.round(v))}</text>`; }
  if (sat != null) g += `<rect x="${L + sat * bw}" y="${T}" width="${R - L - sat * bw}" height="${B - T}" fill="${css('--bad')}" opacity=".07"/>
      <text x="${R - 4}" y="${T + 12}" text-anchor="end" style="fill:${css('--bad')};font-weight:600">⚠ breaking point</text>`;
  if (abortMs && abortMs <= hi) g += `<line x1="${L}" x2="${R}" y1="${yL(abortMs)}" y2="${yL(abortMs)}" stroke="${css('--bad')}" stroke-dasharray="5 4"/>
      <text x="${L + 6}" y="${yL(abortMs) - 5}" style="fill:${css('--bad')}">stop if p99 &gt; ${ms(abortMs)}</text>`;
  S.forEach((s, i) => {
    const cx = L + bw * (i + .5);
    if (A.rps) g += `<rect x="${cx - bw * .28}" y="${yR(s.rps)}" width="${bw * .56}" height="${B - yR(s.rps)}" rx="5" fill="${css('--rps')}"/>
      <text x="${cx}" y="${Math.min(yR(s.rps) + 16, B - 4)}" text-anchor="middle" style="fill:${css('--fg')};font-weight:600">${fmtN(Math.round(s.rps))}/s</text>`;
    g += `<text x="${cx}" y="${B + 18}" text-anchor="middle" style="fill:${css('--fg')}">${s.concurrency} at once</text><text x="${cx}" y="${B + 32}" text-anchor="middle">stage ${s.stage + 1}</text>`;
  });
  for (const [k, c] of [['p50', '--p50'], ['p95', '--p95'], ['p99', '--p99']]) if (A[k]) {
    const pts = S.map((s, i) => [L + bw * (i + .5), yL(s[k + 'Us'] / 1000)]);
    g += polyline(pts, css(c), 2.5) + pts.map(([x, y]) => `<circle cx="${x}" cy="${y}" r="4" fill="${css('--card')}" stroke="${css(c)}" stroke-width="2.5"/>`).join('');
  }
  g += `<text x="${L - 6}" y="${T - 4}" text-anchor="end">req/s</text><text x="${R + 6}" y="${T - 4}">latency</text>`;
  g += S.map((s, i) => `<rect data-i="${i}" x="${L + bw * i}" y="${T}" width="${bw}" height="${B - T}" fill="transparent"/>`).join('');
  svg.innerHTML = g;
  svg.onmousemove = (e) => {
    const i = e.target.dataset?.i; if (i == null) return hideTip();
    const s = S[i], fr = failRate(s);
    showTip(e, `<b>Stage ${s.stage + 1} · ${s.concurrency} at once</b><br>${Math.round(s.rps).toLocaleString()} req/s<br>p50 ${fmtUs(s.p50Us)} · p95 ${fmtUs(s.p95Us)} · p99 ${fmtUs(s.p99Us)}<br>${fr ? `${pct(fr)} failed` : 'no failures'}`);
  };
  svg.onmouseleave = hideTip;
}

// ---------- B: over time ----------
function drawB() {
  const { d, series, B: o } = state.report, S = d.summaries, svg = $('#ch-b');
  const W = 760, H = 290, L = 46, R = 740, T = 22, BOT = 210, EL = 228, EB = 262;
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  const all = series.flat();
  if (!all.length) { svg.innerHTML = `<text x="${L}" y="40">No per-request data stored for this run.</text>`; return; }
  const lats = all.flatMap(p => [p.p50Us, p.p99Us]).map(u => u / 1000);
  const lo = Math.max(0.01, Math.min(...lats) / 1.5), hi = Math.max(...lats) * 1.5, yL = logScale(lo, hi, BOT, T);
  const total = S.reduce((a, s) => a + Math.max(s.durationMs, 1), 0);
  const widths = S.map(s => o.scale === 'equal' ? (R - L) / S.length : (R - L) * Math.max(s.durationMs, 1) / total);
  const maxFail = Math.max(...all.map(p => (p.c5xx + p.errors) / p.count), 0.0001);
  let x0 = L, g = '', errBars = '', hits = '';
  const lines = { p50: [], p95: [], p99: [] };
  for (const v of logTicks(lo, hi)) g += `<line x1="${L}" x2="${R}" y1="${yL(v)}" y2="${yL(v)}" stroke="${css('--grid')}"/><text x="${L - 6}" y="${yL(v) + 4}" text-anchor="end">${ms(v)}</text>`;
  S.forEach((s, si) => {
    const w = widths[si], dur = Math.max(s.durationMs, 1), bad = !stageHealthy(d, s);
    g += `<rect x="${x0}" y="${T}" width="${w}" height="${EB - T}" fill="${si % 2 ? css('--track') : 'transparent'}" opacity=".7"/>`;
    if (w > 34) g += `<text x="${x0 + 4}" y="${T - 6}" style="fill:${bad ? css('--bad') : css('--muted')}">${w > 110 ? `stage ${s.stage + 1} · ${s.concurrency} at once` : `S${s.stage + 1}`}</text>`;
    for (const p of series[si]) {
      const x = x0 + Math.min(p.offsetMs / dur, 1) * w;
      for (const q of ['p50', 'p95', 'p99']) lines[q].push([x, yL(p[q + 'Us'] / 1000)]);
      const f = (p.c5xx + p.errors) / p.count;
      if (o.errs && f) errBars += `<rect x="${x}" y="${EB - f / maxFail * (EB - EL)}" width="${Math.max(1.2, w / 60)}" height="${f / maxFail * (EB - EL)}" fill="${css('--bad')}"/>`;
      hits += `<rect data-s="${si}" data-k="${series[si].indexOf(p)}" x="${x}" y="${T}" width="${Math.max(w / 60, 1)}" height="${EB - T}" fill="transparent"/>`;
    }
    for (const q in lines) lines[q].push(null); // break the line between stages
    x0 += w;
  });
  for (const [q, c] of [['p50', '--p50'], ['p95', '--p95'], ['p99', '--p99']]) if (o[q]) {
    let seg = [];
    for (const pt of lines[q]) { if (pt) seg.push(pt); else { g += polyline(seg, css(c), 1.8); seg = []; } }
  }
  g += `<line x1="${L}" x2="${R}" y1="${EB}" y2="${EB}" stroke="${css('--border')}"/>`;
  if (o.errs) g += `<text x="${L - 6}" y="${EB - 4}" text-anchor="end">fails</text>` + errBars;
  svg.innerHTML = g + `<text x="${R}" y="${EB + 18}" text-anchor="end">${o.scale === 'real' ? `${fmtT(total / 1000)} of load →` : 'each stage gets equal width →'}</text>` + hits;
  svg.onmousemove = (e) => {
    const t = e.target.dataset; if (t.s == null) return hideTip();
    const p = series[t.s][t.k], f = (p.c5xx + p.errors) / p.count;
    showTip(e, `<b>Stage ${S[t.s].stage + 1}</b>, ${(p.offsetMs / 1000).toFixed(1)}s in · ${p.count.toLocaleString()} requests<br>p50 ${fmtUs(p.p50Us)} · p95 ${fmtUs(p.p95Us)} · p99 ${fmtUs(p.p99Us)}${f ? `<br>${pct(f)} failed` : ''}`);
  };
  svg.onmouseleave = hideTip;
}

// ---------- C: spread ----------
async function drawC() {
  const R = state.report, { d, C } = R, stage = C.stage, s = d.summaries[stage], svg = $('#ch-c');
  const W = 760, H = 260, L = 46, RR = 740, T = 16, BOT = 222;
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  if (!(stage in R.hist)) R.hist[stage] = (await getJSON(`/api/runs/${d.id}/hist?stage=${s.stage}`)) || [];
  if (state.report !== R || C.stage !== stage) return; // superseded while loading
  const bars = R.hist[stage];
  if (!bars.length) { svg.innerHTML = `<text x="${L}" y="40">No per-request data stored for this stage.</text>`; return; }
  const lo = bars[0].fromUs / 1000, hi = bars[bars.length - 1].toUs / 1000;
  const xOf = hi / lo > 1.01 ? logScale(lo, hi, L, RR) : (v) => L + (RR - L) / 2;
  const tot = bars.reduce((a, b) => a + b.count, 0), max = Math.max(...bars.map(b => b.count));
  let cum = 0;
  const cdf = bars.map(b => (cum += b.count) / tot);
  let g = '';
  for (const v of logTicks(lo, hi)) g += `<line x1="${xOf(v)}" x2="${xOf(v)}" y1="${T}" y2="${BOT}" stroke="${css('--grid')}"/><text x="${xOf(v)}" y="${BOT + 16}" text-anchor="middle">${ms(v)}</text>`;
  if (C.mode === 'hist') g += bars.map(b => {
    const x1 = xOf(b.fromUs / 1000), x2 = xOf(b.toUs / 1000), h = b.count / max * (BOT - T);
    return `<rect x="${x1 + .5}" y="${BOT - h}" width="${Math.max(x2 - x1 - 1, 1)}" height="${h}" rx="2" fill="${css('--accent')}" opacity=".8"/>`;
  }).join('');
  else {
    g += polyline(bars.map((b, i) => [xOf(b.toUs / 1000), BOT - cdf[i] * (BOT - T)]), css('--accent'), 2.5);
    for (const v of [.5, .9, .99]) g += `<line x1="${L}" x2="${RR}" y1="${BOT - v * (BOT - T)}" y2="${BOT - v * (BOT - T)}" stroke="${css('--grid')}"/><text x="${L - 6}" y="${BOT - v * (BOT - T) + 4}" text-anchor="end">${v * 100}%</text>`;
  }
  [['p50', '--p50'], ['p95', '--p95'], ['p99', '--p99']].forEach(([k, c], j) => {
    const x = xOf(s[k + 'Us'] / 1000);
    g += `<line x1="${x}" x2="${x}" y1="${T}" y2="${BOT}" stroke="${css(c)}" stroke-width="2" stroke-dasharray="4 3"/>
          <text x="${x + 4}" y="${T + 10 + j * 14}" style="fill:${css(c)};font-weight:600">${k} ${fmtUs(s[k + 'Us'])}</text>`;
  });
  g += `<line id="ch-c-cur" x1="0" x2="0" y1="${T}" y2="${BOT}" stroke="${css('--fg')}" opacity="0"/>`;
  svg.innerHTML = g;
  svg.onmousemove = (e) => {
    const x = svgX(svg, e);
    const i = bars.findIndex(b => xOf(b.toUs / 1000) >= x);
    if (x < L || x > RR || i < 0) return hideTip();
    const cur = $('#ch-c-cur'); cur.setAttribute('x1', x); cur.setAttribute('x2', x); cur.setAttribute('opacity', .4);
    showTip(e, `<b>${(cdf[i] * 100).toFixed(1)}%</b> of requests finished within <b>${fmtUs(bars[i].toUs)}</b><br>${(100 - cdf[i] * 100).toFixed(1)}% took longer`);
  };
  svg.onmouseleave = () => { hideTip(); const cur = $('#ch-c-cur'); if (cur) cur.setAttribute('opacity', 0); };
}

// ---------- D: response types ----------
function drawD() {
  const { d, series } = state.report, S = d.summaries, svg = $('#ch-d');
  const W = 760, H = 170, L = 46, R = 740, T = 8, BOT = 140, sw = (R - L) / S.length;
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  let g = '';
  S.forEach((s, si) => {
    const pts = series[si], bw = sw / Math.max(pts.length, 1);
    pts.forEach((p, k) => {
      let y = BOT;
      for (const [v, c] of [[p.ok, '--good'], [p.c4xx, '--c4xx'], [p.c5xx, '--bad'], [p.errors, '--cerr']]) {
        if (!v) continue;
        const h = v / p.count * (BOT - T); y -= h;
        g += `<rect x="${L + si * sw + k * bw}" y="${y}" width="${bw + .3}" height="${h}" fill="${css(c)}" opacity="${c === '--good' ? .55 : .95}"/>`;
      }
    });
    g += `<line x1="${L + si * sw}" x2="${L + si * sw}" y1="${T}" y2="${BOT}" stroke="${css('--card')}" stroke-width="2"/><text x="${L + si * sw + sw / 2}" y="${BOT + 16}" text-anchor="middle">stage ${s.stage + 1}</text>`;
    g += `<rect data-i="${si}" x="${L + si * sw}" y="${T}" width="${sw}" height="${BOT - T}" fill="transparent"/>`;
  });
  g += `<text x="${L - 6}" y="${T + 8}" text-anchor="end">100%</text><text x="${L - 6}" y="${BOT}" text-anchor="end">0%</text>`;
  svg.innerHTML = g;
  svg.onmousemove = (e) => {
    const i = e.target.dataset?.i; if (i == null) return hideTip();
    const s = S[i], n = served(s), by = (lo, hi) => Object.entries(s.codes || {}).reduce((a, [c, v]) => a + (+c >= lo && +c < hi ? v : 0), 0);
    showTip(e, `<b>Stage ${s.stage + 1}</b> · ${n.toLocaleString()} responses<br>success ${pct(by(0, 400) / n)} · 4xx ${pct(by(400, 500) / n)}<br>5xx ${pct(by(500, 1000) / n)} · errors ${pct(s.errorCount / n)}`);
  };
  svg.onmouseleave = hideTip;
}

// ---------- E: compare ----------
async function drawE() {
  const R = state.report, svg = $('#ch-e');
  if (!svg || R.prevId == null) return;
  const id = R.prevId;
  if (!(id in R.compare)) R.compare[id] = await getJSON(`/api/runs/${id}`);
  if (R.prevId !== id) return; // user picked another run while loading
  const prev = R.compare[id]?.summaries || [], S = R.d.summaries;
  const n = Math.min(S.length, prev.length), row = 42, L = 90, RR = 520, T = 10;
  svg.setAttribute('viewBox', `0 0 760 ${T + row * Math.max(n, 1) + 24}`);
  if (!n) { svg.innerHTML = `<text x="0" y="24">Run ${id} has no finished stages to compare.</text>`; return; }
  const all = [...S.slice(0, n), ...prev.slice(0, n)].flatMap(s => [s.p50Us, s.p99Us]).map(u => u / 1000);
  const lo = Math.max(0.01, Math.min(...all) / 1.5), hi = Math.max(...all) * 1.5, xOf = logScale(lo, hi, L, RR);
  let g = '';
  for (const v of logTicks(lo, hi)) g += `<line x1="${xOf(v)}" x2="${xOf(v)}" y1="${T}" y2="${T + row * n}" stroke="${css('--grid')}"/><text x="${xOf(v)}" y="${T + row * n + 14}" text-anchor="middle">${ms(v)}</text>`;
  for (let i = 0; i < n; i++) {
    const s = S[i], p = prev[i], y = T + row * i + row / 2, dd = s.p99Us / p.p99Us - 1;
    const same = s.concurrency === p.concurrency && s.requests === p.requests;
    g += `<text x="${L - 12}" y="${y + 4}" text-anchor="end" style="fill:${css('--fg')}">S${i + 1} · ${s.concurrency}</text>
      <line x1="${xOf(p.p50Us / 1000)}" x2="${xOf(p.p99Us / 1000)}" y1="${y - 6}" y2="${y - 6}" stroke="${css('--prev')}" stroke-width="6" stroke-linecap="round"/>
      <line x1="${xOf(s.p50Us / 1000)}" x2="${xOf(s.p99Us / 1000)}" y1="${y + 6}" y2="${y + 6}" stroke="${css('--accent')}" stroke-width="6" stroke-linecap="round"/>
      <text x="${RR + 16}" y="${y + 4}" style="fill:${Math.abs(dd) < .05 ? css('--muted') : dd > 0 ? css('--bad') : css('--good')};font-weight:600">p99 ${Math.abs(dd) < .05 ? '≈' : dd > 0 ? '▲' : '▼'} ${Math.round(Math.abs(dd) * 100)}%  (${fmtUs(p.p99Us)} → ${fmtUs(s.p99Us)})${same ? '' : ' · different plan'}</text>`;
  }
  svg.innerHTML = g;
}

// =====================================================================
// History
// =====================================================================
async function loadHistory() {
  const runs = ((await getJSON('/api/runs')) || []).filter(r => r.status !== 'pending');
  $('#history-table tbody').innerHTML = runs.length ? runs.map(r => {
    const p = r.peak, v = r.verdict;
    const result = r.status === 'running' ? '<span class="muted">in progress…</span>'
      : !p ? '<span class="muted">no stage finished</span>'
      : v && v.saturationStage != null ? `<span class="bad">● breaks at stage ${v.saturationStage + 1}</span>`
      : '<span class="good">● healthy</span>';
    return `<tr class="clickable" onclick="go('#run/${r.id}')">
     <td>${r.id}</td><td>${new Date(r.createdAt).toLocaleString()}</td>
     <td class="curl-cell muted" title="${esc(redact(r.curl))}">${esc(redact(r.curl))}</td>
     <td><span class="status s-${r.status}">${r.status}</span></td>
     <td>${p ? r.stages : '–'}</td>
     <td>${v && v.maxHealthyRps ? Math.round(v.maxHealthyRps).toLocaleString() : '–'}</td>
     <td>${p ? fmtUs(p.p99Us) : '–'}</td><td>${result}</td></tr>`;
  }).join('') : '<tr><td colspan="8" class="muted">No runs yet. Start one from “New run”.</td></tr>';
}

let savedTheme = 'light';
try { savedTheme = localStorage.getItem('apiperf-theme') || 'light'; } catch {}
renderPlan();
syncRules();
applyTheme(savedTheme);
route();
