/* View renderers. Each takes a container and a slice of state, and writes
 * markup. None of them own state or fetch anything — app.js does that, so a
 * view can always be re-rendered from scratch against the latest snapshot. */

import {
  esc, usd, grams, num, compact, ms, pct, bytes, duration, clock, clockMs,
  cloudLabel, cloudColor, cloudDot, verdictBadge, icon, cssVar,
} from './util.js';
import { sparkline, radar, contributionBar, allocationBars, shareRibbon } from './charts.js';

/* ── KPIs ──────────────────────────────────────────────────────────────── */

export function renderKPIs(el, ov) {
  const k = ov.kpis ?? {};
  const savingsPositive = (k.savingsUsdPerHour ?? 0) >= 0;

  const tiles = [
    {
      cls: 'kpi--ok',
      label: 'Decision rate',
      value: num(k.decisionsPerSecond ?? 0, 1),
      unit: '/s',
      foot: `<strong>${compact(k.totalDecisions ?? 0)}</strong> lifetime · ${pct(k.allowRate ?? 0, 1)} allowed`,
    },
    {
      cls: savingsPositive ? 'kpi--cost' : 'kpi--bad',
      label: 'Egress savings',
      value: usd(k.savingsUsdPerHour ?? 0, { sign: true }),
      unit: '/hr',
      foot: `<strong>${usd(k.savedUsd ?? 0, { sign: true })}</strong> so far · ${usd(k.projectedAnnualUsd ?? 0)}/yr at this rate`,
    },
    {
      cls: 'kpi--carbon',
      label: 'Carbon avoided',
      value: grams(k.savedGrams ?? 0, { sign: true }),
      unit: '',
      foot: `${grams(k.savingsGramsPerHour ?? 0, { sign: true })}/hr · ≈${num(k.equivalentKmDriven ?? 0, 0)} km not driven`,
    },
    {
      cls: (k.routesBreachingSlo ?? 0) > 0 ? 'kpi--bad' : 'kpi--latency',
      label: 'Blended p95',
      value: ms(k.blendedP95Ms ?? 0),
      unit: '',
      foot: (k.routesBreachingSlo ?? 0) > 0
        ? `<strong style="color:var(--bad)">${k.routesBreachingSlo} route(s) over SLO</strong>`
        : `all routes inside SLO · ${ms(k.latencyDebtMs ?? 0)} traded away total`,
    },
    {
      cls: (k.openBreakers ?? 0) > 0 ? 'kpi--bad' : 'kpi--ok',
      label: 'Fleet health',
      value: `${k.healthyBackends ?? 0}/${k.totalBackends ?? 0}`,
      unit: '',
      foot: (k.openBreakers ?? 0) > 0
        ? `<strong style="color:var(--bad)">${k.openBreakers} circuit(s) open</strong>`
        : `${bytes(k.bytesRouted ?? 0)} routed · ${k.staleSignals ?? 0} stale signal(s)`,
    },
    {
      cls: 'kpi--latency',
      label: 'Decision cost',
      value: num(k.decisionMeanUs ?? 0, 1),
      unit: 'µs',
      foot: `peak ${num(k.decisionMaxUs ?? 0, 0)}µs · policy cache ${pct(k.policyCacheHitRate ?? 0, 0)}`,
    },
  ];

  el.innerHTML = tiles.map((t) => `
    <article class="kpi ${t.cls}">
      <div class="kpi__label">${esc(t.label)}</div>
      <div class="kpi__value">${esc(t.value)}<span class="kpi__unit">${esc(t.unit)}</span></div>
      <div class="kpi__foot">${t.foot}</div>
    </article>`).join('');
}

/* ── Alert chips ───────────────────────────────────────────────────────── */

export function renderAlerts(el, ov) {
  const k = ov.kpis ?? {};
  const chips = [];

  if (ov.status?.demoMode) {
    chips.push(`<span class="chip chip--mute">${icon('flask')} simulated fleet</span>`);
  }
  if ((k.openBreakers ?? 0) > 0) {
    chips.push(`<span class="chip chip--bad">${icon('alert')} ${k.openBreakers} circuit open</span>`);
  }
  if ((k.routesBreachingSlo ?? 0) > 0) {
    chips.push(`<span class="chip chip--warn">${icon('clock')} ${k.routesBreachingSlo} over SLO</span>`);
  }
  if ((k.staleSignals ?? 0) > 0) {
    chips.push(`<span class="chip chip--warn">${icon('alert')} ${k.staleSignals} stale quote</span>`);
  }
  const active = (ov.incidents ?? []).filter((i) => i.active).length;
  if (active > 0) {
    chips.push(`<span class="chip chip--warn">${icon('zap')} ${active} incident${active > 1 ? 's' : ''}</span>`);
  }
  if (!chips.length) {
    chips.push(`<span class="chip chip--ok">${icon('check')} nominal</span>`);
  }
  el.innerHTML = chips.join('');
}

/* ── Stream chart ──────────────────────────────────────────────────────── */

const CLOUD_ORDER = ['aws', 'gcp', 'azure', 'onprem'];

export function streamSeries(ov) {
  const series = [];
  for (const c of CLOUD_ORDER) {
    const pts = ov.series?.[`rps.${c}`];
    if (!pts?.length) continue;
    series.push({ key: c, label: cloudLabel(c), color: cloudColor(c), points: pts });
  }
  return series;
}

export function renderStreamLegend(el, series, ov) {
  const byCloud = new Map((ov.clouds ?? []).map((c) => [c.cloud, c]));
  el.innerHTML = series.map((s) => {
    const c = byCloud.get(s.key);
    return `<span class="legend__item">
      <span class="legend__swatch" style="background:${esc(s.color)}"></span>
      ${esc(s.label)} <b class="mono">${num(c?.rps ?? 0, 1)}/s</b>
    </span>`;
  }).join('');
}

/** The accessible alternative to the stacked area chart. */
export function renderStreamTable(table, ov) {
  const rows = (ov.clouds ?? []).map((c) => `
    <tr>
      <td>${cloudDot(c.cloud)}${esc(c.display)}</td>
      <td class="num">${num(c.rps, 1)}</td>
      <td class="num">${pct(c.share / Math.max(1e-9, (ov.clouds ?? []).reduce((a, x) => a + x.share, 0)), 1)}</td>
      <td class="num">${num(c.healthy, 0)}/${num(c.backends, 0)}</td>
      <td class="num">$${c.avgEgressUsdPerGb.toFixed(4)}</td>
      <td class="num">${c.avgCarbonGramsPerGb.toFixed(2)}</td>
      <td class="num">${ms(c.avgLatencyP95Ms)}</td>
    </tr>`).join('');

  table.innerHTML = `
    <thead><tr>
      <th scope="col">Cloud</th><th scope="col" class="num">req/s</th><th scope="col" class="num">Share</th>
      <th scope="col" class="num">Healthy</th><th scope="col" class="num">$/GB</th>
      <th scope="col" class="num">gCO2e/GB</th><th scope="col" class="num">p95</th>
    </tr></thead><tbody>${rows}</tbody>`;
}

/* ── Radar ─────────────────────────────────────────────────────────────── */

export function renderRadar(el, ov) {
  const backends = ov.backends ?? [];
  if (!backends.length) { el.innerHTML = '<div class="empty"><p>No signals yet.</p></div>'; return; }

  const dims = [
    { key: 'cost', label: 'cost', get: (b) => b.egress?.value ?? 0 },
    { key: 'latency', label: 'latency', get: (b) => b.latencyP95?.value ?? 0 },
    { key: 'carbon', label: 'carbon', get: (b) => b.carbonPerGb?.value ?? 0 },
    { key: 'reliability', label: 'errors', get: (b) => b.errorRate?.value ?? 0 },
  ];

  // Normalise across every backend so the rings mean the same thing for each
  // provider polygon.
  const ranges = dims.map((d) => {
    const vals = backends.map(d.get);
    return { lo: Math.min(...vals), hi: Math.max(...vals) };
  });

  const byCloud = new Map();
  for (const b of backends) {
    const c = b.backend.cloud;
    if (!byCloud.has(c)) byCloud.set(c, []);
    byCloud.get(c).push(b);
  }

  const groups = [...byCloud.entries()]
    .sort((a, b) => a[0].localeCompare(b[0]))
    .map(([cloud, list]) => ({
      label: cloudLabel(cloud),
      color: cloudColor(cloud),
      values: dims.map((d, i) => {
        const mean = list.reduce((a, b) => a + d.get(b), 0) / list.length;
        const { lo, hi } = ranges[i];
        return hi - lo < 1e-12 ? 0.5 : (mean - lo) / (hi - lo);
      }),
    }));

  radar(el, groups, dims);
}

/* ── Allocation ────────────────────────────────────────────────────────── */

export function renderRoutePicker(el, ov, selected, onPick) {
  const routes = ov.routes ?? [];
  el.innerHTML = routes.map((r) => `
    <button class="seg__btn ${r.route.id === selected ? 'is-active' : ''}" role="tab"
            aria-selected="${r.route.id === selected}" data-route="${esc(r.route.id)}">
      ${esc(r.route.displayName || r.route.id)}
    </button>`).join('');
  el.querySelectorAll('[data-route]').forEach((btn) => {
    btn.addEventListener('click', () => onPick(btn.dataset.route));
  });
}

export function renderAllocation(el, ov, routeId) {
  const route = (ov.routes ?? []).find((r) => r.route.id === routeId) ?? (ov.routes ?? [])[0];
  if (!route) { el.innerHTML = '<div class="empty"><p>No routes configured.</p></div>'; return; }

  const obj = route.objectives ?? {};
  const weights = ['cost', 'latency', 'carbon', 'reliability'].map((d) =>
    `<span class="legend__item">
       <span class="legend__swatch" style="background:${cssVar(d)}"></span>
       ${d} <b>${pct(obj[d] ?? 0, 0)}</b>
     </span>`).join('');

  const slo = route.route.latencySloMs > 0
    ? `<span class="chip ${route.sloMet ? 'chip--ok' : 'chip--bad'}">
         p95 ${ms(route.projectedP95Ms)} vs ${ms(route.route.latencySloMs)} SLO
       </span>`
    : '<span class="chip chip--mute">no latency SLO</span>';

  el.innerHTML = `
    <div class="toolbar" style="margin-bottom:14px;justify-content:space-between">
      <div class="legend">${weights}</div>
      <div class="chipgroup">
        ${slo}
        <span class="chip chip--mute">gen ${num(route.generation, 0)}</span>
        <span class="chip chip--mute">churn ${num(route.churn, 3)}</span>
      </div>
    </div>
    ${allocationBars(route.candidates ?? [], { rps: route.rps })}`;
}

/* ── Decision feed ─────────────────────────────────────────────────────── */

export function feedRow(d, { animate = false } = {}) {
  const saved = d.savedUsd ?? 0;
  const savedCls = saved >= 0 ? 'feed__save' : 'feed__cost';
  const dest = d.chosenBackend
    ? `${cloudDot(d.cloud)}${esc(d.region || d.chosenBackend)}`
    : '<span class="dim">—</span>';

  return `<li class="feed__item ${animate ? 'feed__item--enter' : ''}" data-id="${esc(d.id)}" role="listitem" tabindex="0">
    ${verdictBadge(d.verdict)}
    <div class="feed__main">
      <div class="feed__path">${esc(d.method)} ${esc(d.path)}</div>
      <div class="feed__meta">${esc(shortSubject(d.subject))} · ${dest}</div>
    </div>
    <div class="feed__right">
      ${d.verdict === 'allow'
        ? `<span class="${savedCls}">${usd(saved, { sign: true })}</span><br>${clock(d.ts)}`
        : `<span class="dim">${clock(d.ts)}</span>`}
    </div>
  </li>`;
}

function shortSubject(id) {
  if (!id) return 'anonymous';
  const m = /spiffe:\/\/([^/]+)\/ns\/([^/]+)\/sa\/(.+)/.exec(id);
  return m ? `${m[2]}/${m[3]}` : id;
}

/* ── Decisions table ───────────────────────────────────────────────────── */

export function renderDecisionRows(tbody, decisions, selectedId) {
  if (!decisions.length) {
    tbody.innerHTML = '<tr><td colspan="8"><div class="empty"><p>No decisions match these filters.</p></div></td></tr>';
    return;
  }
  tbody.innerHTML = decisions.map((d) => {
    const saved = d.savedUsd ?? 0;
    const delta = d.latencyDeltaMs ?? 0;
    return `<tr data-id="${esc(d.id)}" class="${d.id === selectedId ? 'is-selected' : ''}" tabindex="0">
      <td class="mono dim">${clockMs(d.ts)}</td>
      <td>${verdictBadge(d.verdict)}</td>
      <td><span class="trunc" title="${esc(d.subject)}">${esc(shortSubject(d.subject))}</span></td>
      <td><span class="trunc mono" title="${esc(d.path)}">${esc(d.path)}</span></td>
      <td>${d.chosenBackend ? `${cloudDot(d.cloud)}<span class="trunc">${esc(d.chosenBackend)}</span>` : '<span class="dim">—</span>'}</td>
      <td class="num" style="color:${saved >= 0 ? 'var(--ok)' : 'var(--warn)'}">
        ${d.verdict === 'allow' ? usd(saved, { sign: true }) : '<span class="dim">—</span>'}
      </td>
      <td class="num" style="color:${delta > 0 ? 'var(--warn)' : 'var(--text-2)'}">
        ${d.verdict === 'allow' ? `${delta > 0 ? '+' : ''}${delta.toFixed(1)}` : '<span class="dim">—</span>'}
      </td>
      <td class="num dim">${num(d.computeMicros ?? 0, 0)}${d.cached ? '<span title="policy verdict served from cache"> ⌁</span>' : ''}</td>
    </tr>`;
  }).join('');
}

/* ── Decision detail ───────────────────────────────────────────────────── */

export function renderDecisionDetail(el, d) {
  if (!d) {
    el.innerHTML = `<div class="empty"><span class="empty__icon">${icon('pointer')}</span>
      <p>Select a decision to see exactly why it was routed the way it was.</p></div>`;
    return;
  }

  const cands = [...(d.candidates ?? [])].sort((a, b) => {
    if (a.eligible !== b.eligible) return a.eligible ? -1 : 1;
    return (b.weight ?? 0) - (a.weight ?? 0) || (b.score ?? 0) - (a.score ?? 0);
  });

  const candHTML = cands.map((c) => {
    const { bar, legend } = contributionBar(c);
    const won = c.backendId === d.chosenBackend;
    const out = !c.eligible;
    return `<div class="cand ${won ? 'cand--won' : ''} ${out ? 'cand--out' : ''}">
      <div class="cand__head">
        <span class="cand__name">${cloudDot(c.cloud)}<em>${esc(c.backendId)}</em>${won ? ' &nbsp;<span class="badge badge--allow">chosen</span>' : ''}</span>
        <span class="cand__score">score ${(c.score ?? 0).toFixed(3)} · ${pct(c.weight ?? 0, 1)}</span>
      </div>
      <div class="cand__bar">${bar}</div>
      <div class="cand__legend">${legend}</div>
      ${c.reason ? `<div class="cand__reason">${esc(c.reason)}</div>` : ''}
    </div>`;
  }).join('');

  const traceHTML = (d.policyTrace ?? []).map((t) => {
    let cls = 'trace__row--miss';
    if (t.error) cls = 'trace__row--err';
    else if (t.matched && t.effect === 'deny') cls = 'trace__row--deny';
    else if (t.matched) cls = 'trace__row--hit';
    return `<div class="trace__row ${cls}">
      <span class="trace__dot" aria-hidden="true"></span>
      <span>
        <span class="trace__name">${esc(t.policy)}</span>
        <span class="badge badge--mute">${esc(t.effect)}</span>
        ${!t.matched && !t.error ? '<span class="trace__detail"> did not match</span>' : ''}
        ${t.detail ? `<div class="trace__detail">${esc(t.detail)}</div>` : ''}
        ${t.error ? `<div class="trace__detail">${esc(t.error)}</div>` : ''}
      </span>
    </div>`;
  }).join('') || '<p class="card__sub">No policies were evaluated.</p>';

  const counterfactual = d.baselineBackend ? `
    <div class="counterfactual">
      <div class="counterfactual__box">
        <div class="counterfactual__label">Latency-only balancer</div>
        <div class="counterfactual__value">${esc(d.baselineBackend)}</div>
      </div>
      <span class="counterfactual__arrow" aria-hidden="true">→</span>
      <div class="counterfactual__box">
        <div class="counterfactual__label">Sluice chose</div>
        <div class="counterfactual__value">${esc(d.chosenBackend || '—')}</div>
      </div>
    </div>
    <dl class="kv" style="margin-top:12px">
      <dt>Cost</dt><dd style="color:${(d.savedUsd ?? 0) >= 0 ? 'var(--ok)' : 'var(--warn)'}">
        ${usd(d.savedUsd ?? 0, { sign: true })} on ${bytes(d.request?.estimatedBytes ?? 0)}</dd>
      <dt>Carbon</dt><dd style="color:${(d.savedGrams ?? 0) >= 0 ? 'var(--ok)' : 'var(--warn)'}">
        ${grams(d.savedGrams ?? 0, { sign: true })}</dd>
      <dt>Latency</dt><dd style="color:${(d.latencyDeltaMs ?? 0) > 0 ? 'var(--warn)' : 'var(--text-2)'}">
        ${(d.latencyDeltaMs ?? 0) > 0 ? '+' : ''}${(d.latencyDeltaMs ?? 0).toFixed(1)} ms p95</dd>
    </dl>` : '<p class="card__sub">No comparable baseline: no healthy alternative was eligible.</p>';

  el.innerHTML = `
    <div class="detail__head">
      <div class="detail__title">
        ${verdictBadge(d.verdict)}
        <strong>${esc(d.request?.method ?? '')} ${esc(d.request?.path ?? '')}</strong>
      </div>
      <div class="detail__id mono">${esc(d.id)} · ${clockMs(d.ts)} · ${num(d.computeMicros ?? 0, 0)}µs${d.cached ? ' · cached verdict' : ''}</div>
      ${d.denyReason ? `<p class="card__sub" style="color:var(--bad);margin-top:8px">${esc(d.denyReason)}</p>` : ''}
    </div>

    <div class="detail__section">
      <div class="detail__label">Identity</div>
      <dl class="kv">
        <dt>Subject</dt><dd class="mono">${esc(d.subject?.id ?? 'anonymous')}</dd>
        <dt>Trust domain</dt><dd>${esc(d.subject?.trustDomain || '—')}</dd>
        <dt>Transport</dt><dd>${d.subject?.mtls ? 'mutual TLS' : (d.subject?.authenticated ? 'bearer token' : 'unauthenticated')}</dd>
        <dt>Data class</dt><dd>${esc(d.request?.dataClass || 'internal')}</dd>
        <dt>Source</dt><dd class="mono">${esc(d.request?.sourceIp || '—')}</dd>
        <dt>Route</dt><dd>${esc(d.routeId || '—')}</dd>
      </dl>
    </div>

    <div class="detail__section">
      <div class="detail__label">Policy trace</div>
      <div class="trace">${traceHTML}</div>
    </div>

    <div class="detail__section">
      <div class="detail__label">Counterfactual</div>
      ${counterfactual}
    </div>

    <div class="detail__section">
      <div class="detail__label">Score derivation — bar length is the penalty, remainder is the score</div>
      ${candHTML || '<p class="card__sub">No candidates were scored.</p>'}
    </div>`;
}

/* ── Policy ────────────────────────────────────────────────────────────── */

const POLICY_KEYWORDS = /\b(policy|priority|effect|when|require|prefer|message|description|tags)\b/g;
const POLICY_EFFECTS = /\b(allow|deny|constrain|prefer)\b/g;
const POLICY_OPS = /\b(and|or|not|in|matches|startswith|endswith|contains|true|false|null)\b/g;
const POLICY_ATTRS = /\b(subject|request|backend|time)\.[a-z_]+/g;

/**
 * Tokenise a policy document for display.
 *
 * Comments and strings are extracted first and replaced with placeholders, so
 * a keyword inside a message string is not coloured as a keyword. The
 * placeholders use a control character no policy document can contain.
 */
export function highlightPolicy(src) {
  const stash = [];
  const park = (html) => ` ${stash.push(html) - 1} `;

  let out = esc(src)
    .replace(/#[^\n]*/g, (m) => park(`<span class="tok-comment">${m}</span>`))
    .replace(/&quot;(?:[^&]|&(?!quot;))*&quot;/g, (m) => park(`<span class="tok-string">${m}</span>`));

  out = out
    .replace(POLICY_ATTRS, (m) => `<span class="tok-attr">${m}</span>`)
    .replace(POLICY_EFFECTS, (m) => `<span class="tok-effect">${m}</span>`)
    .replace(POLICY_KEYWORDS, (m) => `<span class="tok-key">${m}</span>`)
    .replace(POLICY_OPS, (m) => `<span class="tok-op">${m}</span>`)
    .replace(/\b\d+(?:\.\d+)?\b/g, (m) => `<span class="tok-number">${m}</span>`);

  return out.replace(/ (\d+) /g, (_, i) => stash[Number(i)]);
}

export function renderPolicyRules(el, view) {
  const rules = view.policies ?? [];
  if (!rules.length) { el.innerHTML = '<div class="empty"><p>No policies compiled.</p></div>'; return; }

  el.innerHTML = rules.map((p) => {
    const badgeCls = { allow: 'badge--allow', deny: 'badge--deny' }[p.effect] ?? 'badge--mute';
    return `<div class="rule">
      <div class="rule__head">
        <span class="rule__name">${esc(p.name)}</span>
        <span class="badge ${badgeCls}">${esc(p.effect)}</span>
        <span class="badge badge--mute">p${p.priority}</span>
        ${(p.tags ?? []).map((t) => `<span class="badge badge--mute">${esc(t)}</span>`).join('')}
      </div>
      ${p.description ? `<div class="rule__desc">${esc(p.description)}</div>` : ''}
      ${p.when ? `<div class="rule__expr">when ${esc(p.when)}</div>` : ''}
      ${p.require ? `<div class="rule__expr">require ${esc(p.require)}</div>` : ''}
      ${p.prefer ? `<div class="rule__expr">prefer ${esc(Object.entries(p.prefer).map(([k, v]) => `${k}=${v}`).join(' '))}</div>` : ''}
    </div>`;
  }).join('');
}

export function renderPolicyReference(el, view) {
  const attrs = Object.entries(view.attributes ?? {}).map(([root, list]) => `
    <div class="ref__group">
      <div class="ref__root">${esc(root)}</div>
      <div class="ref__attrs">${list.map((a) => `<span class="ref__attr">${esc(a)}</span>`).join('')}</div>
    </div>`).join('');

  const builtins = Object.entries(view.builtins ?? {})
    .sort((a, b) => a[0].localeCompare(b[0]))
    .map(([, doc]) => `<span class="ref__attr">${esc(doc)}</span>`).join('');

  el.innerHTML = `
    <div class="ref__group">
      <div class="ref__root">semantics</div>
      <p class="card__sub">Deny always wins. At least one allow must match or the request is refused.
      A <code>constrain</code> policy prunes destinations without affecting authorisation;
      a <code>prefer</code> policy reshapes the routing objectives for matching traffic.
      Any evaluation error denies the request.</p>
    </div>
    <div class="ref__group">
      <div class="ref__root">operators</div>
      <div class="ref__attrs">${['==', '!=', '<', '<=', '>', '>=', 'and', 'or', 'not', 'in', 'not in', 'matches', 'startswith', 'endswith', 'contains']
        .map((o) => `<span class="ref__attr">${esc(o)}</span>`).join('')}</div>
      <p class="card__sub" style="margin-top:8px"><code>matches</code> is a glob, not a regular expression —
      operator-authored regexes on the request path are a denial-of-service waiting to happen.</p>
    </div>
    ${attrs}
    <div class="ref__group">
      <div class="ref__root">functions</div>
      <div class="ref__attrs">${builtins}</div>
    </div>`;
}

export function renderBacktest(el, res) {
  if (!res) {
    el.innerHTML = `<div class="empty"><span class="empty__icon">${icon('flask')}</span>
      <p>Replay retained decisions through your edits before applying them.</p></div>`;
    return;
  }
  if (!res.ok) {
    el.innerHTML = `<div class="empty"><span class="empty__icon">${icon('alert')}</span>
      <p style="color:var(--bad)">Line ${res.line ?? '?'}, column ${res.column ?? '?'}<br>${esc(res.error)}</p></div>`;
    return;
  }

  const cells = [
    { n: res.newlyDenied, l: 'newly denied', cls: res.newlyDenied ? 'bt__cell--bad' : '' },
    { n: res.newlyAllowed, l: 'newly allowed', cls: res.newlyAllowed ? 'bt__cell--warn' : '' },
    { n: res.narrowedPool, l: 'pool narrowed', cls: res.narrowedPool ? 'bt__cell--warn' : '' },
    { n: res.widenedPool, l: 'pool widened', cls: res.widenedPool ? 'bt__cell--warn' : '' },
    { n: res.unchanged, l: 'unchanged', cls: 'bt__cell--ok' },
  ].map((c) => `<div class="bt__cell ${c.cls}">
      <div class="bt__num">${num(c.n, 0)}</div><div class="bt__lab">${c.l}</div>
    </div>`).join('');

  const samples = (res.samples ?? []).map((s) => {
    const color = s.change === 'newly-denied' ? 'var(--bad)'
      : s.change === 'newly-allowed' ? 'var(--warn)' : 'var(--text-3)';
    return `<div class="bt__sample">
      <div class="bt__change" style="color:${color}">${esc(s.change)} · ${esc(s.was)} → ${esc(s.now)}</div>
      <div class="mono" style="font-size:11px;color:var(--text-2)">${esc(s.path)}</div>
      <div style="font-size:11px;color:var(--text-3)">${esc(shortSubject(s.subject))}
        ${s.dataClass ? ` · ${esc(s.dataClass)}` : ''} · eligible ${s.eligibleWas}→${s.eligibleNow}</div>
      ${s.reason ? `<div style="font-size:11px;color:var(--warn)">${esc(s.reason)}</div>` : ''}
    </div>`;
  }).join('');

  el.innerHTML = `
    <p class="card__sub" style="margin-bottom:12px">
      Replayed <strong>${num(res.replayed, 0)}</strong> retained decisions through both the live
      document and your ${num(res.policies, 0)} candidate policies
      (<code>${esc(res.hash ?? '')}</code>), so the difference is your edit alone —
      SLO shedding and circuit breakers are held constant.
    </p>
    <div class="bt__grid">${cells}</div>
    ${samples ? `<div class="detail__label">Changed decisions</div>${samples}`
      : '<p class="card__sub">Nothing would change for the retained traffic.</p>'}`;
}

/* ── Signals ───────────────────────────────────────────────────────────── */

export function renderSignalCards(el, ov, histories) {
  const backends = [...(ov.backends ?? [])].sort((a, b) => b.share - a.share);
  if (!backends.length) { el.innerHTML = '<div class="empty"><p>No backends registered.</p></div>'; return; }

  el.innerHTML = backends.map((b) => {
    const id = b.backend.id;
    const h = histories?.[id] ?? {};
    const breaker = b.breaker?.state ?? 'closed';
    const breakerChip = breaker === 'closed'
      ? `<span class="badge badge--allow">${pct(b.share, 1)} share</span>`
      : `<span class="badge ${breaker === 'open' ? 'badge--deny' : 'badge--nocap'}">${esc(breaker.replace('_', '-'))}</span>`;

    const metric = (label, value, points, varName) => `
      <div class="sig__metric">
        <div class="sig__mlabel">${esc(label)}</div>
        <div class="sig__mvalue" style="color:${cssVar(varName)}">${value}</div>
        <div class="sig__spark">${sparkline(points ?? [], { color: cssVar(varName) })}</div>
      </div>`;

    return `<article class="card">
      <div class="sig__head">
        <div>
          <div class="sig__name">${cloudDot(b.backend.cloud)}${esc(b.backend.displayName || id)}</div>
          <div class="sig__region">${esc(b.backend.region)} · ${esc(b.backend.jurisdiction || '—')} · ${esc(b.zone?.name || b.backend.gridZone || 'unmapped grid')}</div>
        </div>
        ${breakerChip}
      </div>
      <div class="sig__metrics">
        ${metric('Egress', `$${(b.egress?.value ?? 0).toFixed(4)}`, h.cost, 'cost')}
        ${metric('p95', ms(b.latencyP95?.value ?? 0), h.latency, 'latency')}
        ${metric('Carbon', `${(b.carbonPerGb?.value ?? 0).toFixed(2)} g`, h.carbon, 'carbon')}
        ${metric('Errors', pct(b.errorRate?.value ?? 0, 2), h.reliability, 'reliability')}
      </div>
      <div class="sig__foot">
        <span>${num(b.rps ?? 0, 1)} req/s · ${bytes(b.bytesOut ?? 0)} out</span>
        <span>${usd(b.spentUsd ?? 0)} · ${grams(b.emittedGrams ?? 0)}</span>
      </div>
    </article>`;
  }).join('');
}

export function renderProvenance(table, ov) {
  const rows = (ov.backends ?? []).flatMap((b) => {
    const now = new Date(ov.now).getTime();
    const entry = (label, q) => {
      const age = q?.asOf ? (now - new Date(q.asOf).getTime()) / 1000 : null;
      const stale = (b.stale ?? []).length > 0 && age != null && age > 120;
      return `<tr>
        <td>${cloudDot(b.backend.cloud)}<span class="mono">${esc(b.backend.id)}</span></td>
        <td>${esc(label)}</td>
        <td class="mono">${esc(q?.source ?? '—')}</td>
        <td class="num ${stale ? '' : 'dim'}" style="${stale ? 'color:var(--warn)' : ''}">
          ${age == null ? '—' : `${duration(age)} ago`}
        </td>
      </tr>`;
    };
    return [entry('egress price', b.egress), entry('grid intensity', b.gridIntensity)];
  }).join('');

  table.innerHTML = `
    <thead><tr>
      <th scope="col">Backend</th><th scope="col">Signal</th>
      <th scope="col">Source</th><th scope="col" class="num">Age</th>
    </tr></thead><tbody>${rows}</tbody>`;
}

/* ── Topology adjacency table ──────────────────────────────────────────── */

export function renderTopologyTable(table, topo) {
  const nodes = new Map((topo.nodes ?? []).map((n) => [n.id, n]));
  const rows = (topo.edges ?? [])
    .filter((e) => nodes.get(e.to)?.kind === 'region')
    .sort((a, b) => b.share - a.share)
    .map((e) => {
      const n = nodes.get(e.to);
      return `<tr>
        <td>${cloudDot(n.cloud)}${esc(n.label)}</td>
        <td class="mono dim">${esc(n.region)}</td>
        <td class="num">${pct(e.share, 1)}</td>
        <td class="num">${num(e.rps, 1)}</td>
        <td class="num">${ms(n.latencyP95Ms)}</td>
        <td class="num">$${(n.egressUsdPerGb ?? 0).toFixed(4)}</td>
        <td class="num">${(n.carbonGramsPerGb ?? 0).toFixed(2)}</td>
        <td>${n.healthy ? '<span class="badge badge--allow">carrying</span>'
          : `<span class="badge badge--deny">${esc(n.breaker === 'open' ? 'ejected' : 'shed')}</span>`}</td>
      </tr>`;
    }).join('');

  table.innerHTML = `
    <thead><tr>
      <th scope="col">Region</th><th scope="col">Zone</th><th scope="col" class="num">Share</th>
      <th scope="col" class="num">req/s</th><th scope="col" class="num">p95</th>
      <th scope="col" class="num">$/GB</th><th scope="col" class="num">gCO2e/GB</th><th scope="col">State</th>
    </tr></thead><tbody>${rows}</tbody>`;
}

/* ── Incident console ──────────────────────────────────────────────────── */

export function renderIncidentList(el, incidents, onResolve) {
  const active = (incidents ?? []).filter((i) => i.active);
  if (!active.length) { el.innerHTML = ''; return; }

  el.innerHTML = active.map((i) => `
    <li class="console__row">
      <span>
        <strong>${esc(i.kind.replace('_', ' '))}</strong><br>
        <span class="dim mono" style="font-size:10.5px">${esc(i.backendId)} · ${Math.round(i.remainingSeconds)}s left</span>
      </span>
      <button class="console__kill" type="button" data-incident="${esc(i.id)}">end</button>
    </li>`).join('');

  el.querySelectorAll('[data-incident]').forEach((b) => {
    b.addEventListener('click', () => onResolve(b.dataset.incident));
  });
}
