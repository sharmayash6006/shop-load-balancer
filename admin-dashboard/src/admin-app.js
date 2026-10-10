'use strict';
// Admin dashboard. All data comes from the load balancer's real runtime state via API_URL/_lb/api/*.
const API = ((window.APP_CONFIG && window.APP_CONFIG.API_URL) || '').replace(/\/+$/, '');
const root = document.getElementById('root');
const S = { snap: null, page: null, es: null, charts: {}, chartTimer: null, range: '5m', sseOk: false };
const PAGES = [['dashboard', 'Dashboard'], ['requests', 'Requests'], ['failures', 'Failed requests'], ['users', 'Users'], ['orders', 'Orders'], ['loadtest', 'Load testing'], ['settings', 'Settings']];

const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const $ = (s, el = document) => el.querySelector(s);
const fmt = (n) => Number(n || 0).toLocaleString('en-IN');
const ms = (n) => (n < 10 ? Number(n).toFixed(1) : Math.round(n)) + 'ms';
const size = (b) => (b >= 1024 ? (b / 1024).toFixed(1) + 'KB' : b + 'B');
const money = (n) => '₹' + Number(n).toLocaleString('en-IN');
const time = (iso) => new Date(iso).toLocaleTimeString([], { hour12: false });
const stClass = (s) => (s >= 500 ? 's5' : s >= 400 ? 's4' : s >= 300 ? 's3' : 's2');
const bshort = (id) => (id ? 'B' + id.replace('backend-', '') : '-');
const toast = (m) => { const t = $('#toast'); t.textContent = m; t.classList.add('show'); clearTimeout(toast.t); toast.t = setTimeout(() => t.classList.remove('show'), 2500); };

async function api(method, path, body) {
  const opts = { method, credentials: 'include', headers: { 'X-Requested-With': 'admin' } };
  if (body !== undefined) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
  const res = await fetch(API + path, opts);
  let data = null;
  try { data = await res.json(); } catch (e) { /* none */ }
  if (res.status === 401 && !path.endsWith('/login')) { stopLive(); go('/admin/login', true); throw new Error('not authenticated'); }
  if (!res.ok) throw new Error((data && data.error) || 'request failed (' + res.status + ')');
  return data;
}

// ---------- router ----------
function go(path, replace) { history[replace ? 'replaceState' : 'pushState'](null, '', path); route(); }
document.addEventListener('click', (e) => { const a = e.target.closest('a[data-link]'); if (a) { e.preventDefault(); go(a.getAttribute('href')); } });
window.addEventListener('popstate', route);

async function route() {
  clearInterval(S.chartTimer);
  S.charts = {};
  let page = location.pathname.replace(/^\/admin\/?/, '').split('/')[0] || 'dashboard';
  if (page === 'login') return loginPage();
  try { await api('GET', '/_lb/api/me'); } catch (e) { return; }
  if (!PAGES.some((p) => p[0] === page)) page = 'dashboard';
  S.page = page;
  root.innerHTML = `<div class="shell"><aside><h1>⚖️ LB ADMIN</h1>${PAGES.map(([k, l]) => `<a href="/admin/${k}" data-link class="${k === page ? 'on' : ''}">${l}</a>`).join('')}
    <div class="foot"><span id="conn"></span><br><br><button id="logout" style="width:100%">Logout</button></div></aside><main id="main"></main></div>`;
  $('#logout').onclick = async () => { stopLive(); await api('POST', '/_lb/logout'); go('/admin/login', true); };
  startLive();
  ({ dashboard, requests: () => requestsPage(false), failures: () => requestsPage(true), users, orders, loadtest, settings })[page]();
  paintConn();
}

function loginPage() {
  stopLive();
  root.innerHTML = `<form class="login" id="f"><h2 style="margin-top:0">Administrator login</h2><label>Username</label><input name="username" autocomplete="username" required>
    <label>Password</label><input name="password" type="password" autocomplete="current-password" required><p class="err" id="err"></p><button class="primary" style="width:100%">Sign in</button></form>`;
  $('#f').onsubmit = async (e) => {
    e.preventDefault();
    try { await api('POST', '/_lb/login', Object.fromEntries(new FormData(e.target))); go('/admin/dashboard', true); } catch (err) { $('#err').textContent = err.message; }
  };
}

// ---------- real-time stream ----------
function startLive() {
  if (S.es) return;
  S.es = new EventSource(API + '/_lb/api/events', { withCredentials: true });
  S.es.addEventListener('snapshot', (e) => { S.sseOk = true; S.snap = JSON.parse(e.data); paintConn(); onSnapshot(); });
  S.es.onerror = () => { S.sseOk = false; paintConn(); };
}
function stopLive() { if (S.es) { S.es.close(); S.es = null; } }
function paintConn() { const c = $('#conn'); if (c) c.innerHTML = `<span class="live ${S.sseOk ? '' : 'off'}"></span>${S.sseOk ? 'Live' : 'Reconnecting…'}`; }
function onSnapshot() {
  if (S.page === 'dashboard') { paintKpis(); paintBackends(); appendLive(); }
  if (S.page === 'loadtest') paintLoad();
}

// ---------- charts (canvas, no libraries) ----------
function drawChart(canvas, labels, series, opt = {}) {
  const dpr = window.devicePixelRatio || 1, w = canvas.clientWidth, h = canvas.clientHeight;
  canvas.width = w * dpr; canvas.height = h * dpr;
  const g = canvas.getContext('2d'); g.scale(dpr, dpr); g.clearRect(0, 0, w, h);
  const pad = { l: 44, r: 8, t: 18, b: 20 }, pw = w - pad.l - pad.r, ph = h - pad.t - pad.b;
  let max = opt.max || 0;
  if (!opt.max) series.forEach((s) => s.data.forEach((v) => { if (v > max) max = v; }));
  max = max || 1;
  g.font = '11px system-ui'; g.fillStyle = '#8a93b2'; g.strokeStyle = '#263057'; g.lineWidth = 1;
  for (let i = 0; i <= 4; i++) {
    const y = pad.t + ph - (ph * i) / 4;
    g.beginPath(); g.moveTo(pad.l, y); g.lineTo(w - pad.r, y); g.stroke();
    g.textAlign = 'right'; g.fillText(opt.int ? Math.round((max * i) / 4) : +((max * i) / 4).toPrecision(3), pad.l - 5, y + 4);
  }
  const n = labels.length;
  g.textAlign = 'center';
  [0, Math.floor((n - 1) / 2), n - 1].forEach((i) => { if (labels[i] !== undefined) g.fillText(labels[i], pad.l + (pw * i) / Math.max(1, n - 1), h - 5); });
  series.forEach((s, si) => {
    g.strokeStyle = s.color; g.fillStyle = s.color + '33'; g.lineWidth = 1.8; g.beginPath();
    s.data.forEach((v, i) => {
      const x = pad.l + (pw * i) / Math.max(1, n - 1), y = pad.t + ph - (ph * v) / max;
      if (opt.step) { if (i === 0) g.moveTo(x, y); else { g.lineTo(x, g._ly); g.lineTo(x, y); } g._ly = y; } else if (i === 0) g.moveTo(x, y); else g.lineTo(x, y);
    });
    g.stroke();
    if (s.fill) { g.lineTo(pad.l + pw, pad.t + ph); g.lineTo(pad.l, pad.t + ph); g.closePath(); g.fill(); }
    g.fillStyle = s.color; g.textAlign = 'left'; g.fillText('● ' + s.name, pad.l + 4 + si * 110, 11);
  });
}

const COLORS = ['#6366f1', '#22c55e', '#f59e0b', '#38bdf8', '#ec4899', '#a78bfa', '#14b8a6', '#f97316'];

async function refreshCharts() {
  if (S.page !== 'dashboard') return;
  const d = await api('GET', '/_lb/api/timeseries?range=' + S.range).catch(() => null);
  if (!d) return;
  const P = d.points, long = S.range === '1h' || S.range === '24h';
  const L = P.map((p) => new Date(p.t * 1000).toLocaleTimeString([], long ? { hour: '2-digit', minute: '2-digit', hour12: false } : { minute: '2-digit', second: '2-digit', hour12: false }));
  const c = (id) => $('#' + id);
  drawChart(c('c-rps'), L, [{ name: 'req/s', color: COLORS[0], data: P.map((p) => p.rps), fill: true }]);
  drawChart(c('c-lat'), L, [{ name: 'avg ms', color: COLORS[1], data: P.map((p) => p.avg_ms) }, { name: 'max ms', color: COLORS[2], data: P.map((p) => p.max_ms) }]);
  drawChart(c('c-ok'), L, [{ name: 'successful', color: COLORS[1], data: P.map((p) => p.ok), fill: true }, { name: 'failed', color: '#ef4444', data: P.map((p) => p.failed), fill: true }], { int: true });
  drawChart(c('c-be'), L, d.backends.map((id, i) => ({ name: id, color: COLORS[i % 8], data: P.map((p) => p.per_backend[i] || 0) })), { int: true });
  drawChart(c('c-act'), L, [{ name: 'active conns', color: COLORS[3], data: P.map((p) => p.active), fill: true }], { int: true });
  drawChart(c('c-http'), L, [['2xx', 's2xx', '#22c55e'], ['3xx', 's3xx', '#38bdf8'], ['4xx', 's4xx', '#f59e0b'], ['5xx', 's5xx', '#ef4444']].map(([n, k, col]) => ({ name: n, color: col, data: P.map((p) => p[k]) })), { int: true });
  drawChart(c('c-health'), L, [{ name: 'healthy backends', color: COLORS[1], data: P.map((p) => p.healthy), fill: true }], { step: true, int: true, max: d.backends.length });
}

// ---------- dashboard ----------
function dashboard() {
  S.liveRows = []; S.lastSeq = 0;
  $('#main').innerHTML = `<div id="banner"></div>
   <div class="bar"><h2 style="margin:0">Load Balancer Dashboard</h2><span class="spacer"></span>
     <label class="muted">Algorithm <select id="alg"></select></label>
     <label class="muted">Charts <select id="range">${['1m', '5m', '15m', '1h', '24h'].map((r) => `<option value="${r}" ${r === S.range ? 'selected' : ''}>Last ${r.replace('m', ' min').replace('h', ' hour').replace('24 hour', '24 hours')}</option>`).join('')}</select></label></div>
   <div class="kpis" id="kpis"></div>
   <h2>Backend servers</h2><div class="panel tablewrap" id="backends"></div>
   <div class="grid2"><div><h2>Charts</h2></div><div></div></div>
   <div class="charts">${[['c-rps', 'Requests per second'], ['c-lat', 'Response time'], ['c-ok', 'Successful vs failed'], ['c-be', 'Traffic per backend'], ['c-act', 'Active connections'], ['c-http', 'HTTP status codes'], ['c-health', 'Backend health']]
     .map(([id, t]) => `<div class="panel"><h3>${t}</h3><canvas id="${id}"></canvas></div>`).join('')}</div>
   <div class="grid2"><div><h2>Backend state changes</h2><div class="panel tablewrap" id="hevents" style="max-height:260px;overflow:auto"></div></div>
   <div><h2>Failure reasons</h2><div class="panel" id="reasons"></div></div></div>
   <h2>Real-time requests</h2><div class="panel tablewrap" style="max-height:420px;overflow:auto"><table><thead><tr><th>Time</th><th>Request ID</th><th>User</th><th>IP</th><th>Method</th><th>Endpoint</th><th>Action</th><th>Detail</th><th>Backend</th><th>Status</th><th>Time</th><th>Result</th></tr></thead><tbody id="live"></tbody></table></div>`;
  api('GET', '/_lb/api/config').then((c) => {
    $('#alg').innerHTML = c.algorithms.map((a) => `<option value="${a}" ${a === c.settings.algorithm ? 'selected' : ''}>${a.replace(/_/g, ' ').replace(/\b\w/g, (m) => m.toUpperCase())}</option>`).join('');
  });
  $('#alg').onchange = async (e) => { await api('POST', '/_lb/api/config', { algorithm: e.target.value }); toast('Algorithm changed — applies to new requests immediately'); };
  $('#range').onchange = (e) => { S.range = e.target.value; refreshCharts(); };
  refreshCharts(); S.chartTimer = setInterval(() => { refreshCharts(); refreshEvents(); }, 2000); refreshEvents();
  if (S.snap) onSnapshot();
}

function paintKpis() {
  const s = S.snap.stats, el = $('#kpis'); if (!el) return;
  $('#banner').innerHTML = s.no_healthy_backends ? '<div class="alert">⚠ NO HEALTHY BACKENDS — all requests are being answered with HTTP 503</div>' : '';
  const k = (l, v) => `<div class="kpi"><span>${l}</span><b>${v}</b></div>`;
  el.innerHTML = k('Total requests', fmt(s.total_requests)) + k('Successful', fmt(s.successful)) + k('Failed', fmt(s.failed)) + k('Success rate', s.success_rate.toFixed(2) + '%') +
    k('Avg response', ms(s.avg_ms)) + k('P50 / P95 / P99', `${ms(s.p50_ms)} / ${ms(s.p95_ms)} / ${ms(s.p99_ms)}`) + k('Max latency', ms(s.max_ms)) + k('Requests/sec', s.requests_per_sec.toFixed(1)) +
    k('Requests/min', fmt(s.requests_per_min)) + k('Active connections', fmt(s.active_connections)) + k('Availability', s.availability_pct.toFixed(2) + '%') +
    k('Healthy backends', `${s.healthy_backends} / ${s.total_backends}`) + k('2xx · 3xx', `${fmt(s.s2xx)} · ${fmt(s.s3xx)}`) + k('4xx · 5xx', `${fmt(s.s4xx)} · ${fmt(s.s5xx)}`) +
    k('Timeouts', fmt(s.timeouts)) + k('Conn. errors', fmt(s.connection_errors)) + k('Rate limited', fmt(s.rate_limited)) + k('Retried requests', fmt(s.retried_requests));
  const r = Object.entries(s.failure_reasons || {}).sort((a, b) => b[1] - a[1]);
  $('#reasons').innerHTML = r.length ? r.map(([k2, v]) => `<div class="dist"><span style="width:200px">${esc(k2)}</span><div class="track"><div class="fill" style="width:${(v / r[0][1]) * 100}%;background:#ef4444"></div></div><b>${fmt(v)}</b></div>`).join('') : '<span class="muted">No failures recorded.</span>';
}

function paintBackends() {
  const el = $('#backends'); if (!el || el.contains(document.activeElement) && document.activeElement.tagName === 'INPUT') return;
  const tot = S.snap.backends.reduce((a, b) => a + b.total_requests, 0) || 1;
  el.innerHTML = `<table><thead><tr><th>Server</th><th>Status</th><th>Port</th><th>Weight</th><th>Active</th><th>Requests</th><th>Success</th><th>Failed</th><th>Traffic</th><th>Avg response</th><th>Last health check</th><th>Last error</th><th>Controls</th></tr></thead><tbody>
  ${S.snap.backends.map((b) => `<tr><td><b>${esc(b.name)}</b></td><td><span class="tag ${b.status}">${b.status}</span></td><td>${b.port}</td>
   <td><input type="number" min="1" max="100" value="${b.weight}" id="w-${b.id}"><button data-w="${b.id}">Set</button></td><td>${b.active_connections}</td><td>${fmt(b.total_requests)}</td><td>${fmt(b.successful_requests)}</td><td>${fmt(b.failed_requests)}</td>
   <td>${((b.total_requests / tot) * 100).toFixed(1)}%</td><td>${ms(b.avg_response_ms)}</td><td>${b.last_health_check ? time(b.last_health_check) + ' (' + ms(b.last_check_latency_ms) + ')' : '-'}</td><td class="muted">${esc(b.last_error || '-')}</td>
   <td>${b.enabled ? `<button class="danger" data-act="disable:${b.id}">Disable</button>` : `<button class="primary" data-act="enable:${b.id}">Enable</button>`} <button data-act="recover:${b.id}">Recover</button></td></tr>`).join('')}</tbody></table>`;
  el.querySelectorAll('[data-act]').forEach((b) => b.onclick = async () => { const [a, id] = b.dataset.act.split(':'); await api('POST', `/_lb/api/backends/${id}/${a}`); toast(`${id}: ${a}`); });
  el.querySelectorAll('[data-w]').forEach((b) => b.onclick = async () => { try { await api('POST', `/_lb/api/backends/${b.dataset.w}/weight`, { weight: Number($('#w-' + b.dataset.w).value) }); toast('Weight updated'); } catch (e) { toast(e.message); } });
}

async function refreshEvents() {
  const el = $('#hevents'); if (!el) return;
  const ev = await api('GET', '/_lb/api/health-events').catch(() => []);
  el.innerHTML = ev.length ? `<table><thead><tr><th>Time</th><th>Backend</th><th>Transition</th><th>Source</th><th>Reason</th></tr></thead><tbody>${ev.map((e) => `<tr><td>${time(e.time)}</td><td>${esc(e.backend_id)}</td><td><span class="tag ${e.from}">${e.from}</span> → <span class="tag ${e.to}">${e.to}</span></td><td>${esc(e.source)}</td><td class="muted">${esc(e.reason)}</td></tr>`).join('')}</tbody></table>` : '<span class="muted">No state changes since the load balancer started.</span>';
}

function liveRow(e) {
  return `<tr><td>${time(e.time)}</td><td class="mono">${esc(e.request_id)}</td><td>${esc(e.user_name || '-')}</td><td>${esc(e.ip)}</td><td>${esc(e.method)}</td><td>${esc(e.path)}</td><td>${esc(e.action)}</td>
  <td class="muted">${esc(e.order_id ? e.order_id + ' ' : '')}${esc(e.detail)}</td><td>${bshort(e.backend_id)}${e.retries ? ` <span class="tag s4">retry ${e.retries}</span>` : ''}</td><td><span class="tag ${stClass(e.status)}">${e.status}</span></td><td>${ms(e.latency_ms)}</td>
  <td>${e.success ? '<span class="tag s2">OK</span>' : `<span class="tag fail">${esc(e.failure_reason || 'FAILED')}</span>`}</td></tr>`;
}
function appendLive() {
  const tb = $('#live'); if (!tb || !S.snap.recent.length) return;
  tb.insertAdjacentHTML('afterbegin', S.snap.recent.slice().reverse().map(liveRow).join(''));
  while (tb.rows.length > 60) tb.deleteRow(-1);
}

// ---------- requests / failures ----------
function requestsPage(failedOnly) {
  $('#main').innerHTML = `<h2 style="margin-top:0">${failedOnly ? 'Failed request analysis' : 'Request activity'}</h2>
  <form class="bar" id="f"><input name="q" placeholder="Search: user, request ID, IP, order ID, endpoint, product, backend…" style="min-width:340px;flex:1">
  <select name="status"><option value="">Any status</option><option value="2xx">2xx</option><option value="3xx">3xx</option><option value="4xx">4xx</option><option value="5xx">5xx</option><option value="error">≥ 400</option></select>
  <select name="backend"><option value="">Any backend</option>${(S.snap ? S.snap.backends : []).map((b) => `<option value="${b.id}">${esc(b.name)}</option>`).join('')}</select>
  <select name="loadtest"><option value="">Real + load test</option><option value="exclude">Exclude load test</option><option value="only">Load test only</option></select>
  ${failedOnly ? '' : '<label class="muted"><input type="checkbox" name="failed" value="1"> failed only</label>'}<button class="primary">Search</button></form>
  ${failedOnly ? '<div class="panel" id="reasonbox" style="margin-bottom:12px"></div>' : ''}<div class="panel tablewrap" id="res"></div>`;
  const run = async () => {
    const f = new FormData($('#f')); const p = new URLSearchParams();
    for (const [k, v] of f) if (v) p.set(k, v);
    if (failedOnly) p.set('failed', '1');
    p.set('limit', '300');
    try {
      const rows = await api('GET', '/_lb/api/requests?' + p);
      $('#res').innerHTML = reqTable(rows, failedOnly);
    } catch (e) { $('#res').innerHTML = `<span class="err">${esc(e.message)}</span>`; }
    if (failedOnly && S.snap) {
      const r = Object.entries(S.snap.stats.failure_reasons || {}).sort((a, b) => b[1] - a[1]);
      $('#reasonbox').innerHTML = '<h3>Why requests failed (since start)</h3>' + (r.map(([k, v]) => `<span class="tag fail" style="margin-right:8px">${esc(k)}: ${fmt(v)}</span>`).join('') || '<span class="muted">None</span>');
    }
  };
  $('#f').onsubmit = (e) => { e.preventDefault(); run(); };
  run();
}
function reqTable(rows, failed) {
  if (!rows.length) return '<span class="muted">No matching requests.</span>';
  return `<table><thead><tr><th>Timestamp</th><th>Request ID</th><th>User</th><th>IP address</th><th>Method</th><th>Endpoint</th><th>Action</th><th>Backend</th><th>Status</th><th>Response time</th><th>Req size</th><th>Resp size</th><th>Result</th><th>Failure reason</th></tr></thead><tbody>
  ${rows.map((e) => `<tr><td>${new Date(e.time).toLocaleString([], { hour12: false })}</td><td class="mono">${esc(e.request_id)}</td><td>${esc(e.user_name || '-')}${e.user_id ? ` <span class="muted">USR-${e.user_id}</span>` : ''}</td><td>${esc(e.ip)}</td><td>${esc(e.method)}</td><td>${esc(e.path)}</td><td>${esc(e.action)}</td>
  <td>${esc(e.backend_id || '-')}${e.retries ? ` <span class="tag s4">retry ${e.retries}</span>` : ''}</td><td><span class="tag ${stClass(e.status)}">${e.status}</span></td><td>${ms(e.latency_ms)}</td><td>${size(e.request_size)}</td><td>${size(e.response_size)}</td>
  <td>${e.success ? '<span class="tag s2">SUCCESS</span>' : '<span class="tag fail">FAILURE</span>'}</td><td>${esc(e.failure_reason || '-')}</td></tr>`).join('')}</tbody></table>`;
}

// ---------- users ----------
function users() {
  $('#main').innerHTML = `<h2 style="margin-top:0">Users &amp; activity</h2><form class="bar" id="f"><input name="q" placeholder="Search user name, email or USR-id" style="min-width:320px"><button class="primary">Search</button></form>
  <div class="grid2"><div class="panel tablewrap" id="ul"></div><div class="panel" id="act"><span class="muted">Select a user to see their journey.</span></div></div>`;
  const run = async () => {
    const rows = await api('GET', '/_lb/api/users?q=' + encodeURIComponent($('#f').q.value));
    $('#ul').innerHTML = rows.length ? `<table><thead><tr><th>User</th><th>Email</th><th>Orders</th><th>Requests</th><th>Last activity</th></tr></thead><tbody>${rows.map((u) =>
      `<tr class="click" data-u="${u.id}"><td><b>${esc(u.name)}</b> <span class="muted">${u.label}</span></td><td>${esc(u.email)}</td><td>${u.orders}</td><td>${fmt(u.requests)}</td><td>${u.last_activity ? new Date(u.last_activity).toLocaleString([], { hour12: false }) : '-'}</td></tr>`).join('')}</tbody></table>` : '<span class="muted">No users.</span>';
    $('#ul').querySelectorAll('[data-u]').forEach((tr) => tr.onclick = () => showActivity(tr.dataset.u, rows.find((u) => String(u.id) === tr.dataset.u)));
  };
  $('#f').onsubmit = (e) => { e.preventDefault(); run(); }; run();
}
async function showActivity(id, u) {
  const rows = (await api('GET', `/_lb/api/users/${id}/activity`)).reverse();
  $('#act').innerHTML = `<h3>${esc(u.name)} · ${u.label}</h3>` + (rows.length ? `<div class="timeline">${rows.map((e) => `<div class="ev ${e.success ? '' : 'bad'}"><b>${esc(e.action)}</b> ${esc(e.detail)} ${e.order_id ? `<span class="tag s2">${esc(e.order_id)}</span>` : ''}<br>
    <span class="muted">${new Date(e.time).toLocaleTimeString([], { hour12: false })} · ${esc(e.method)} ${esc(e.path)} · ${esc(e.backend_id || '-')} · ${e.status} · ${ms(e.latency_ms)} · <span class="mono">${esc(e.request_id)}</span></span></div>`).join('')}</div>` : '<span class="muted">No recorded requests.</span>');
}

// ---------- orders ----------
function orders() {
  $('#main').innerHTML = `<h2 style="margin-top:0">Orders</h2><form class="bar" id="f"><input name="q" placeholder="Search order ID, user, product, request ID, backend" style="min-width:340px"><button class="primary">Search</button></form><div class="panel tablewrap" id="ol"></div>`;
  const run = async () => {
    const rows = await api('GET', '/_lb/api/orders?q=' + encodeURIComponent($('#f').q.value));
    $('#ol').innerHTML = rows.length ? `<table><thead><tr><th>Order ID</th><th>User</th><th>Product</th><th>Qty</th><th>Amount</th><th>Order status</th><th>Request ID</th><th>Backend</th><th>Timestamp</th></tr></thead><tbody>${rows.map((o) =>
      `<tr class="click" data-o="${esc(o.order_id)}"><td><b>${esc(o.order_id)}</b></td><td>${esc(o.user)}</td><td>${esc(o.items)}</td><td>${o.quantity}</td><td>${money(o.amount)}</td><td><span class="tag s2">${esc(o.status)}</span></td><td class="mono">${esc(o.request_id)}</td><td>${esc(o.backend_id)}</td><td>${new Date(o.created_at).toLocaleString([], { hour12: false })}</td></tr>`).join('')}</tbody></table>` : '<span class="muted">No orders.</span>';
    $('#ol').querySelectorAll('[data-o]').forEach((tr) => tr.onclick = () => trace(tr.dataset.o));
  };
  $('#f').onsubmit = (e) => { e.preventDefault(); run(); }; run();
}
async function trace(id) {
  const t = await api('GET', `/_lb/api/orders/${encodeURIComponent(id)}/trace`);
  const o = t.order, r = t.order_request, m = document.createElement('div');
  m.className = 'modal'; m.onclick = (e) => { if (e.target === m) m.remove(); };
  m.innerHTML = `<div class="panel"><div class="bar"><h2 style="margin:0">Order ${esc(o.order_id)}</h2><span class="spacer"></span><button id="x">Close</button></div>
   <p><b>User:</b> ${esc(o.user)} (USR-${o.user_id}) · <b>Amount:</b> ${money(o.amount)} · <span class="tag s2">${esc(o.status)}</span> <span class="tag s2">${esc(t.payment_status)}</span><br><b>Ship to:</b> ${esc(t.shipping_address)}</p>
   <h3>Items</h3>${t.line_items.map((i) => `<div>${esc(i.product)} × ${i.quantity} @ ${money(i.unit_price)}</div>`).join('')}
   <h3 style="margin-top:14px">Timeline</h3><div class="timeline">${t.timeline.map((e) => `<div class="ev ${e.success ? '' : 'bad'}"><b>${time(e.time)}</b> ${esc(e.method)} ${esc(e.path)} — ${esc(e.action)} <span class="muted">${esc(e.detail)}</span> <span class="tag ${stClass(e.status)}">${e.status}</span></div>`).join('') || '<span class="muted">No earlier requests recorded.</span>'}</div>
   <h3 style="margin-top:14px">Order request</h3><table><tbody><tr><td>Request ID</td><td class="mono">${esc(o.request_id)}</td></tr><tr><td>Selected backend</td><td>${esc(r ? r.backend_id : o.backend_id)}</td></tr>
   <tr><td>Response</td><td>${r ? `${r.status} ${r.status === 201 ? 'Created' : ''}` : 'not in request log'}</td></tr><tr><td>Latency</td><td>${r ? ms(r.latency_ms) : '-'}</td></tr><tr><td>Retries</td><td>${r ? r.retries : '-'}</td></tr>
   <tr><td>Database</td><td><span class="tag ${t.database === 'SUCCESS' ? 's2' : 'fail'}">${esc(t.database)}</span></td></tr></tbody></table></div>`;
  document.body.appendChild(m); $('#x', m).onclick = () => m.remove();
}

// ---------- load testing ----------
function loadtest() {
  $('#main').innerHTML = `<h2 style="margin-top:0">Load testing <span id="ltstate" class="tag DISABLED">idle</span></h2>
  <p class="muted">Metrics below are computed from the real requests the load balancer receives from k6 (tagged with the <span class="mono">X-LoadTest</span> header). Start a test with: <span class="mono">docker compose run --rm k6 run /tests/ecommerce-test.js</span></p>
  <div class="kpis" id="lt-kpis"></div><div class="charts" style="margin-top:12px">${[['l-rps', 'Requests per second'], ['l-vu', 'Active virtual users'], ['l-lat', 'Average response time'], ['l-act', 'Active connections'], ['l-err', 'Errors per second']].map(([id, t]) => `<div class="panel"><h3>${t}</h3><canvas id="${id}"></canvas></div>`).join('')}
  <div class="panel"><h3>Traffic distribution</h3><div id="lt-dist"></div></div></div>`;
  if (S.snap) paintLoad();
}
function paintLoad() {
  const l = S.snap.load_test, k = $('#lt-kpis'); if (!k) return;
  const st = $('#ltstate'); st.className = 'tag ' + (l.running ? 'HEALTHY' : 'DISABLED'); st.textContent = l.running ? 'running · ' + l.name : l.requests ? 'finished · ' + l.name : 'idle';
  const c = (a, b) => `<div class="kpi"><span>${a}</span><b>${b}</b></div>`;
  k.innerHTML = c('Virtual users', l.virtual_users + ' (peak ' + l.max_virtual_users + ')') + c('Requests', fmt(l.requests)) + c('Requests/sec', l.requests_per_sec.toFixed(1)) + c('Success rate', l.success_rate.toFixed(2) + '%') + c('Failure rate', l.failure_rate.toFixed(2) + '%') +
    c('Avg response', ms(l.avg_ms)) + c('P95', ms(l.p95_ms)) + c('P99', ms(l.p99_ms)) + c('HTTP 2xx', fmt(l.s2xx)) + c('HTTP 4xx', fmt(l.s4xx)) + c('HTTP 5xx', fmt(l.s5xx)) + c('Elapsed', l.elapsed_sec + 's');
  const P = l.series, L = P.map((p) => new Date(p.t * 1000).toLocaleTimeString([], { minute: '2-digit', second: '2-digit', hour12: false }));
  drawChart($('#l-rps'), L, [{ name: 'req/s', color: COLORS[0], data: P.map((p) => p.requests), fill: true }]);
  drawChart($('#l-vu'), L, [{ name: 'VUs', color: COLORS[4], data: P.map((p) => p.vus), fill: true }], { int: true });
  drawChart($('#l-lat'), L, [{ name: 'avg ms', color: COLORS[1], data: P.map((p) => p.avg_ms) }]);
  drawChart($('#l-act'), L, [{ name: 'active', color: COLORS[3], data: P.map((p) => p.active), fill: true }], { int: true });
  drawChart($('#l-err'), L, [{ name: 'failed/s', color: '#ef4444', data: P.map((p) => p.failed), fill: true }], { int: true });
  $('#lt-dist').innerHTML = l.traffic_distribution.map((b) => `<div class="dist"><span style="width:90px">${esc(b.backend_id)}</span><div class="track"><div class="fill" style="width:${b.pct}%"></div></div><b>${b.pct.toFixed(1)}%</b></div>`).join('') || '<span class="muted">No load-test traffic yet.</span>';
}

// ---------- settings ----------
async function settings() {
  const c = await api('GET', '/_lb/api/config'), s = c.settings;
  const f = (k, label, step = 1) => `<label class="muted">${label}<br><input name="${k}" type="number" step="${step}" value="${s[k]}" style="width:180px"></label>`;
  $('#main').innerHTML = `<h2 style="margin-top:0">Configuration</h2><p class="muted">Changes apply immediately, without restarting anything.</p><form class="panel" id="f" style="display:grid;gap:14px;max-width:560px">
   <label class="muted">Algorithm<br><select name="algorithm" style="width:260px">${c.algorithms.map((a) => `<option ${a === s.algorithm ? 'selected' : ''}>${a}</option>`).join('')}</select></label>
   ${f('connect_timeout_ms', 'Connection timeout (ms)')}${f('request_timeout_ms', 'Request timeout, total (ms)')}${f('backend_timeout_ms', 'Backend timeout per attempt (ms)')}${f('retry_count', 'Retry count')}
   ${f('rate_limit', 'Rate limit (requests/second per IP, 0 = off)', 'any')}${f('health_interval_ms', 'Health check interval (ms)')}<button class="primary" style="width:160px">Apply</button></form>`;
  $('#f').onsubmit = async (e) => {
    e.preventDefault();
    const b = {}; for (const [k, v] of new FormData(e.target)) b[k] = k === 'algorithm' ? v : Number(v);
    try { await api('POST', '/_lb/api/config', b); toast('Configuration applied'); } catch (err) { toast(err.message); }
  };
}

route();
