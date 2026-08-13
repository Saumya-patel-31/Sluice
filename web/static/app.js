/* Sluice dashboard — state, transport and view orchestration.
 *
 * One SSE connection carries everything that changes on its own: a snapshot
 * every control-loop tick and batched decisions in between. Views that need
 * more (a decision's full derivation, the policy document) fetch it on demand.
 * Nothing polls on a timer that the stream already covers.
 */

import {
  esc, num, duration, hydrateIcons, throttle, icon,
} from './util.js';
import { StreamChart } from './charts.js';
import { TopologyView } from './topology.js';
import * as V from './views.js';

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

/* ── State ─────────────────────────────────────────────────────────────── */

const state = {
  view: 'overview',
  overview: null,
  feed: [],
  ledger: [],
  selectedDecision: null,
  detail: null,
  live: true,
  allocRoute: null,
  topoRoute: null,
  topoOverlay: 'traffic',
  topology: null,
  policy: null,
  backtest: null,
  histories: {},
  policyDirty: false,
};

const VIEW_META = {
  overview:  { title: 'Overview',  sub: 'Live routing across three clouds' },
  topology:  { title: 'Topology',  sub: 'Where traffic is actually going, and why some paths are dark' },
  decisions: { title: 'Decisions', sub: 'Every decision, with the derivation that produced it' },
  policy:    { title: 'Policy',    sub: 'Zero-trust rules. Backtest before you apply.' },
  signals:   { title: 'Signals',   sub: 'The measurements the router optimises over, and how fresh they are' },
};

let stream = null;
let topology = null;
let timers = [];

/* ── Boot ──────────────────────────────────────────────────────────────── */

function boot() {
  hydrateIcons();
  wireNav();
  wireDecisionFilters();
  wirePolicyEditor();
  wireTopologyControls();
  wireConsole();

  stream = new StreamChart($('#stream-chart'));
  topology = new TopologyView($('#topo-canvas'), $('#topo-tip'));

  connect();
  route();
  window.addEventListener('hashchange', route);
}

/* ── Transport ─────────────────────────────────────────────────────────── */

let source = null;
let retry = 0;

function connect() {
  source?.close();
  setConn('connecting');

  source = new EventSource('/api/stream?points=180');

  source.addEventListener('open', () => { retry = 0; setConn('live'); });

  source.addEventListener('overview', (e) => {
    try {
      state.overview = JSON.parse(e.data);
      setConn('live');
      onOverview();
    } catch (err) {
      console.error('overview parse failed', err);
    }
  });

  source.addEventListener('decisions', (e) => {
    try {
      const payload = JSON.parse(e.data);
      onDecisions(payload.decisions ?? []);
    } catch (err) {
      console.error('decision parse failed', err);
    }
  });

  source.addEventListener('error', () => {
    setConn('down');
    source?.close();
    // Exponential backoff with a ceiling. EventSource reconnects on its own,
    // but only for transport errors; an explicit close needs its own retry.
    retry = Math.min(retry + 1, 6);
    const wait = Math.min(15000, 500 * 2 ** retry);
    setTimeout(connect, wait);
  });
}

function setConn(stateName) {
  const el = $('#conn');
  el.dataset.state = stateName;
  $('.conn__label', el).textContent =
    stateName === 'live' ? 'live' : stateName === 'down' ? 'reconnecting' : 'connecting';
}

/* ── Data arrival ──────────────────────────────────────────────────────── */

function onOverview() {
  const ov = state.overview;
  if (!ov) return;

  $('#rail-policy-hash').textContent = ov.status?.policyHash ?? '—';
  $('#rail-uptime').textContent = duration(ov.status?.uptimeSeconds ?? 0);
  V.renderAlerts($('#alert-chips'), ov);
  hydrateIcons($('#alert-chips'));

  $('#console').hidden = !ov.status?.demoMode;

  if (!state.allocRoute && ov.routes?.length) state.allocRoute = ov.routes[0].route.id;
  if (!state.topoRoute && ov.routes?.length) {
    state.topoRoute = ov.routes.reduce((a, b) => (b.rps > a.rps ? b : a), ov.routes[0]).route.id;
  }

  populateFilterOptions(ov);

  switch (state.view) {
    case 'overview': renderOverview(); break;
    case 'signals': renderSignals(); break;
    case 'topology': renderTopologyControls(); break;
    default: break;
  }

  if (ov.status?.demoMode) {
    V.renderIncidentList($('#inc-active'), ov.incidents, resolveIncident);
  }
}

function onDecisions(list) {
  if (!list.length) return;

  state.feed = [...list.reverse(), ...state.feed].slice(0, 60);
  if (state.view === 'overview') appendFeed(list);

  if (state.view === 'decisions' && state.live) {
    state.ledger = [...list, ...state.ledger].slice(0, 400);
    renderDecisionsTable();
  }
}

/* ── Routing ───────────────────────────────────────────────────────────── */

function route() {
  const name = (location.hash.replace(/^#\/?/, '') || 'overview').split('?')[0];
  const view = VIEW_META[name] ? name : 'overview';
  state.view = view;

  timers.forEach(clearInterval);
  timers = [];

  $$('.page').forEach((p) => { p.hidden = p.dataset.page !== view; });
  $$('.navitem').forEach((a) => {
    const on = a.dataset.view === view;
    a.classList.toggle('is-active', on);
    if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  });

  $('#view-title').textContent = VIEW_META[view].title;
  $('#view-sub').textContent = VIEW_META[view].sub;
  document.title = `${VIEW_META[view].title} · Sluice`;

  switch (view) {
    case 'overview': renderOverview(); break;
    case 'topology': enterTopology(); break;
    case 'decisions': enterDecisions(); break;
    case 'policy': enterPolicy(); break;
    case 'signals': enterSignals(); break;
  }
}

function wireNav() {
  $$('.navitem').forEach((a) => {
    a.addEventListener('click', (e) => {
      e.preventDefault();
      location.hash = a.getAttribute('href');
    });
  });
}

/* ── Overview ──────────────────────────────────────────────────────────── */

function renderOverview() {
  const ov = state.overview;
  if (!ov) return;

  V.renderKPIs($('#kpis'), ov);

  const series = V.streamSeries(ov);
  stream.setSeries(series);
  V.renderStreamLegend($('#stream-legend'), series, ov);
  V.renderStreamTable($('#stream-table'), ov);

  V.renderRadar($('#radar'), ov);

  V.renderRoutePicker($('#route-picker'), ov, state.allocRoute, (id) => {
    state.allocRoute = id;
    renderOverview();
  });
  V.renderAllocation($('#allocation'), ov, state.allocRoute);

  const feed = $('#feed');
  if (!feed.children.length && state.feed.length) {
    feed.innerHTML = state.feed.slice(0, 24).map((d) => V.feedRow(d)).join('');
    wireFeed();
  }
}

function appendFeed(list) {
  const feed = $('#feed');
  if (!feed || $('.page[data-page="overview"]').hidden) return;

  const html = list.slice(-8).reverse().map((d) => V.feedRow(d, { animate: true })).join('');
  feed.insertAdjacentHTML('afterbegin', html);
  while (feed.children.length > 24) feed.lastElementChild.remove();
  wireFeed();
}

function wireFeed() {
  $$('#feed .feed__item').forEach((li) => {
    if (li.dataset.wired === '1') return;
    li.dataset.wired = '1';
    const open = () => { location.hash = '#/decisions'; setTimeout(() => selectDecision(li.dataset.id), 60); };
    li.addEventListener('click', open);
    li.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); } });
  });
}

/* ── Topology ──────────────────────────────────────────────────────────── */

function enterTopology() {
  renderTopologyControls();
  fetchTopology();
  timers.push(setInterval(fetchTopology, 2000));
}

function renderTopologyControls() {
  const ov = state.overview;
  if (!ov?.routes?.length) return;
  const el = $('#topology-route');
  const current = state.topoRoute;
  const wanted = ov.routes.map((r) => r.route.id).join(',');
  if (el.dataset.routes !== wanted || el.dataset.sel !== current) {
    el.dataset.routes = wanted;
    el.dataset.sel = current;
    el.innerHTML = ov.routes.map((r) => `
      <button class="seg__btn ${r.route.id === current ? 'is-active' : ''}" role="tab"
              aria-selected="${r.route.id === current}" data-route="${esc(r.route.id)}">
        ${esc(r.route.displayName || r.route.id)}
      </button>`).join('');
    $$('[data-route]', el).forEach((b) => {
      b.addEventListener('click', () => { state.topoRoute = b.dataset.route; renderTopologyControls(); fetchTopology(); });
    });
  }
}

function wireTopologyControls() {
  $$('#topology-overlay .seg__btn').forEach((b) => {
    b.addEventListener('click', () => {
      $$('#topology-overlay .seg__btn').forEach((x) => {
        x.classList.toggle('is-active', x === b);
        x.setAttribute('aria-selected', String(x === b));
      });
      state.topoOverlay = b.dataset.overlay;
      topology.setOverlay(state.topoOverlay);
    });
  });
}

async function fetchTopology() {
  if (state.view !== 'topology') return;
  try {
    const q = state.topoRoute ? `?route=${encodeURIComponent(state.topoRoute)}` : '';
    const res = await fetch(`/api/topology${q}`);
    if (!res.ok) return;
    state.topology = await res.json();
    topology.setData(state.topology);
    V.renderTopologyTable($('#topo-table'), state.topology);
  } catch (err) {
    console.error('topology fetch failed', err);
  }
}

/* ── Decisions ─────────────────────────────────────────────────────────── */

function enterDecisions() {
  fetchDecisions();
}

function wireDecisionFilters() {
  $('#f-apply').addEventListener('click', fetchDecisions);
  $('#f-subject').addEventListener('keydown', (e) => { if (e.key === 'Enter') fetchDecisions(); });
  ['#f-verdict', '#f-cloud', '#f-route'].forEach((s) => $(s).addEventListener('change', fetchDecisions));
  $('#f-live').addEventListener('change', (e) => {
    state.live = e.target.checked;
    if (state.live) fetchDecisions();
  });
}

function populateFilterOptions(ov) {
  const fill = (sel, values, label) => {
    const el = $(sel);
    const wanted = values.join(',');
    if (el.dataset.values === wanted) return;
    el.dataset.values = wanted;
    const current = el.value;
    el.innerHTML = `<option value="">All</option>` +
      values.map((v) => `<option value="${esc(v)}">${esc(label ? label(v) : v)}</option>`).join('');
    el.value = current;
  };
  fill('#f-cloud', (ov.clouds ?? []).map((c) => c.cloud), (c) => c.toUpperCase());
  fill('#f-route', (ov.routes ?? []).map((r) => r.route.id));
}

async function fetchDecisions() {
  const params = new URLSearchParams({ limit: '250' });
  const verdict = $('#f-verdict').value;
  const cloud = $('#f-cloud').value;
  const routeId = $('#f-route').value;
  const q = $('#f-subject').value.trim();

  if (verdict) params.set('verdict', verdict);
  if (cloud) params.set('cloud', cloud);
  if (routeId) params.set('route', routeId);
  // One search box, two fields: a leading slash reads as a path, anything
  // else as an identity. Two boxes for this would be pedantry.
  if (q) params.set(q.startsWith('/') ? 'path' : 'subject', q);

  try {
    const res = await fetch(`/api/decisions?${params}`);
    if (!res.ok) return;
    const data = await res.json();
    state.ledger = data.decisions ?? [];
    renderDecisionsTable();
  } catch (err) {
    console.error('decision fetch failed', err);
  }
}

const renderDecisionsTable = throttle(() => {
  V.renderDecisionRows($('#decisions-body'), state.ledger, state.selectedDecision);
  $$('#decisions-body tr[data-id]').forEach((tr) => {
    const open = () => selectDecision(tr.dataset.id);
    tr.addEventListener('click', open);
    tr.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); open(); } });
  });
}, 250);

async function selectDecision(id) {
  if (!id) return;
  state.selectedDecision = id;
  $$('#decisions-body tr').forEach((tr) => tr.classList.toggle('is-selected', tr.dataset.id === id));

  try {
    const res = await fetch(`/api/decisions/${encodeURIComponent(id)}`);
    if (!res.ok) {
      $('#decision-detail').innerHTML =
        `<div class="empty"><p>This decision has aged out of the ledger.</p></div>`;
      return;
    }
    state.detail = await res.json();
    V.renderDecisionDetail($('#decision-detail'), state.detail);
    hydrateIcons($('#decision-detail'));
  } catch (err) {
    console.error('decision detail failed', err);
  }
}

/* ── Policy ────────────────────────────────────────────────────────────── */

function enterPolicy() {
  if (state.policy && state.policyDirty) return;
  fetchPolicy();
}

async function fetchPolicy() {
  try {
    const res = await fetch('/api/policy');
    if (!res.ok) return;
    state.policy = await res.json();
    $('#policy-source').value = state.policy.source ?? '';
    state.policyDirty = false;
    syncEditor();
    V.renderPolicyRules($('#policy-rules-panel'), state.policy);
    V.renderPolicyReference($('#policy-reference-panel'), state.policy);
    setPolicyStatus(`loaded ${state.policy.policies?.length ?? 0} policies · ${state.policy.hash}`, '');
  } catch (err) {
    console.error('policy fetch failed', err);
  }
}

function wirePolicyEditor() {
  const ta = $('#policy-source');
  const hl = $('#policy-highlight');
  const gutter = $('#policy-gutter');

  const sync = throttle(syncEditor, 40);
  ta.addEventListener('input', () => { state.policyDirty = true; sync(); });
  ta.addEventListener('scroll', () => {
    hl.scrollTop = ta.scrollTop;
    hl.scrollLeft = ta.scrollLeft;
    gutter.scrollTop = ta.scrollTop;
  });

  // Tab inserts two spaces rather than moving focus. The editor is the point
  // of this view, so trapping Tab is right — Escape still releases it.
  ta.addEventListener('keydown', (e) => {
    if (e.key !== 'Tab' || e.shiftKey) return;
    e.preventDefault();
    const { selectionStart: s, selectionEnd: t, value } = ta;
    ta.value = `${value.slice(0, s)}  ${value.slice(t)}`;
    ta.selectionStart = ta.selectionEnd = s + 2;
    state.policyDirty = true;
    syncEditor();
  });

  $('#policy-backtest').addEventListener('click', runBacktest);
  $('#policy-apply').addEventListener('click', applyPolicy);

  $$('.tabs__btn').forEach((b) => {
    b.addEventListener('click', () => {
      $$('.tabs__btn').forEach((x) => {
        x.classList.toggle('is-active', x === b);
        x.setAttribute('aria-selected', String(x === b));
      });
      $$('[data-tabpanel]').forEach((p) => { p.hidden = p.dataset.tabpanel !== b.dataset.tab; });
    });
  });
}

function syncEditor() {
  const ta = $('#policy-source');
  const src = ta.value;
  $('#policy-highlight').innerHTML = V.highlightPolicy(src) + '\n';
  const lines = src.split('\n').length;
  $('#policy-gutter').textContent = Array.from({ length: lines }, (_, i) => i + 1).join('\n');
}

function setPolicyStatus(text, cls) {
  const el = $('#policy-status');
  el.textContent = text;
  el.className = `status ${cls ? `status--${cls}` : ''}`;
}

async function runBacktest() {
  setPolicyStatus('replaying retained decisions…', 'busy');
  V.renderBacktest($('#policy-backtest-panel'), null);
  selectTab('backtest');

  try {
    const res = await fetch('/api/policy/backtest?limit=800', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ source: $('#policy-source').value }),
    });
    state.backtest = await res.json();
    V.renderBacktest($('#policy-backtest-panel'), state.backtest);
    hydrateIcons($('#policy-backtest-panel'));

    if (!state.backtest.ok) {
      setPolicyStatus(`line ${state.backtest.line}: ${state.backtest.error}`, 'bad');
    } else {
      const changed = state.backtest.newlyDenied + state.backtest.newlyAllowed +
        state.backtest.narrowedPool + state.backtest.widenedPool;
      setPolicyStatus(
        changed === 0
          ? `compiles · no change across ${state.backtest.replayed} replayed decisions`
          : `compiles · ${changed} of ${state.backtest.replayed} decisions would change`,
        changed === 0 ? 'ok' : 'busy');
    }
  } catch (err) {
    setPolicyStatus(`backtest failed: ${err}`, 'bad');
  }
}

async function applyPolicy() {
  const btn = $('#policy-apply');
  btn.disabled = true;
  setPolicyStatus('compiling and installing…', 'busy');
  try {
    const res = await fetch('/api/policy', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ source: $('#policy-source').value }),
    });
    const data = await res.json();
    if (!res.ok) {
      setPolicyStatus(data.line ? `line ${data.line}: ${data.message ?? data.error}` : (data.error ?? 'rejected'), 'bad');
      return;
    }
    state.policyDirty = false;
    setPolicyStatus(`installed · ${data.policies} policies · ${data.hash}`, 'ok');
    await fetchPolicy();
  } catch (err) {
    setPolicyStatus(`apply failed: ${err}`, 'bad');
  } finally {
    btn.disabled = false;
  }
}

function selectTab(name) {
  $$('.tabs__btn').forEach((x) => {
    const on = x.dataset.tab === name;
    x.classList.toggle('is-active', on);
    x.setAttribute('aria-selected', String(on));
  });
  $$('[data-tabpanel]').forEach((p) => { p.hidden = p.dataset.tabpanel !== name; });
}

/* ── Signals ───────────────────────────────────────────────────────────── */

function enterSignals() {
  renderSignals();
  fetchHistories();
  timers.push(setInterval(fetchHistories, 5000));
}

function renderSignals() {
  const ov = state.overview;
  if (!ov) return;
  V.renderSignalCards($('#signal-cards'), ov, state.histories);
  V.renderProvenance($('#provenance-table'), ov);
}

async function fetchHistories() {
  const ov = state.overview;
  if (!ov?.backends?.length || state.view !== 'signals') return;

  // Sequential rather than parallel: this is a background refresh for a view
  // that is already readable, and twelve simultaneous requests would compete
  // with the event stream for the browser's connection budget.
  for (const b of ov.backends) {
    try {
      const res = await fetch(`/api/backends/${encodeURIComponent(b.backend.id)}/history?points=60`);
      if (res.ok) state.histories[b.backend.id] = await res.json();
    } catch { /* a missing sparkline is not worth surfacing */ }
  }
  if (state.view === 'signals') renderSignals();
}

/* ── Incident console ──────────────────────────────────────────────────── */

function wireConsole() {
  const toggle = $('#console-toggle');
  const body = $('#console-body');

  toggle.addEventListener('click', () => {
    const open = body.hidden;
    body.hidden = !open;
    toggle.setAttribute('aria-expanded', String(open));
    if (open) populateIncidentBackends();
  });

  const slider = $('#inc-seconds');
  slider.addEventListener('input', () => { $('#inc-seconds-out').textContent = `${slider.value}s`; });

  $('#inc-fire').addEventListener('click', injectIncident);
}

function populateIncidentBackends() {
  const sel = $('#inc-backend');
  const ov = state.overview;
  if (!ov?.backends?.length || sel.options.length) return;
  sel.innerHTML = ov.backends.map((b) =>
    `<option value="${esc(b.backend.id)}">${esc(b.backend.displayName || b.backend.id)}</option>`).join('');
}

async function injectIncident() {
  const btn = $('#inc-fire');
  btn.disabled = true;
  try {
    await fetch('/api/incidents', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        backendId: $('#inc-backend').value,
        kind: $('#inc-kind').value,
        seconds: Number($('#inc-seconds').value),
        note: 'injected from the dashboard',
      }),
    });
  } catch (err) {
    console.error('incident injection failed', err);
  } finally {
    btn.disabled = false;
  }
}

async function resolveIncident(id) {
  try {
    await fetch(`/api/incidents/${encodeURIComponent(id)}`, { method: 'DELETE' });
  } catch (err) {
    console.error('incident resolve failed', err);
  }
}

/* ── Go ────────────────────────────────────────────────────────────────── */

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', boot);
} else {
  boot();
}
