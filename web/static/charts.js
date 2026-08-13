/* Chart primitives, hand-drawn in SVG and canvas.
 *
 * No charting library. The dashboard ships inside a Go binary under a content
 * security policy that forbids every external origin, and the five chart types
 * it needs are each a few dozen lines. A library would add a build step, a
 * supply-chain dependency and a megabyte, to draw shapes we can draw directly.
 */

import { cssVar, cloudColor, cloudLabel, esc, ms, usd, num, reducedMotion } from './util.js';

/* ── Sparkline ─────────────────────────────────────────────────────────── */

/**
 * A single-series trend line, sized by its container.
 *
 * The y-axis is padded by 8% of the range so a flat series does not collapse
 * onto the baseline and read as zero.
 */
export function sparkline(points, { color = '#7AA7FF', height = 26, fill = true } = {}) {
  if (!points || points.length < 2) {
    return `<svg viewBox="0 0 100 ${height}" preserveAspectRatio="none"></svg>`;
  }
  const vals = points.map((p) => p.v);
  let lo = Math.min(...vals);
  let hi = Math.max(...vals);
  const span = hi - lo;
  if (span < 1e-9) { lo -= Math.abs(lo || 1) * 0.1; hi += Math.abs(hi || 1) * 0.1; }
  else { lo -= span * 0.08; hi += span * 0.08; }

  const w = 100;
  const stepX = w / (points.length - 1);
  const y = (v) => height - ((v - lo) / (hi - lo)) * height;

  let d = '';
  points.forEach((p, i) => { d += `${i === 0 ? 'M' : 'L'}${(i * stepX).toFixed(2)},${y(p.v).toFixed(2)}`; });

  const area = fill
    ? `<path d="${d}L${w},${height}L0,${height}Z" fill="${esc(color)}" opacity=".12"/>`
    : '';
  const lastY = y(points[points.length - 1].v).toFixed(2);

  return `<svg viewBox="0 0 ${w} ${height}" preserveAspectRatio="none" aria-hidden="true">
    ${area}
    <path d="${d}" fill="none" stroke="${esc(color)}" stroke-width="1.5"
          vector-effect="non-scaling-stroke" stroke-linejoin="round" stroke-linecap="round"/>
    <circle cx="${w}" cy="${lastY}" r="2" fill="${esc(color)}" vector-effect="non-scaling-stroke"/>
  </svg>`;
}

/* ── Stacked stream chart ──────────────────────────────────────────────── */

/**
 * StreamChart draws stacked areas on a canvas.
 *
 * Canvas rather than SVG because this redraws once per second over a
 * six-hundred-point window across four series; that is a couple of thousand
 * DOM nodes to reconcile per tick in SVG, and none of them are interactive.
 */
export class StreamChart {
  constructor(canvas) {
    this.canvas = canvas;
    this.ctx = canvas.getContext('2d');
    this.series = [];
    this.hover = null;
    this._resize = this._resize.bind(this);

    this._observer = new ResizeObserver(this._resize);
    this._observer.observe(canvas.parentElement ?? canvas);

    canvas.addEventListener('pointermove', (e) => {
      const r = canvas.getBoundingClientRect();
      this.hover = { x: e.clientX - r.left, y: e.clientY - r.top };
      this.draw();
    });
    canvas.addEventListener('pointerleave', () => { this.hover = null; this.draw(); });
    this._resize();
  }

  destroy() { this._observer.disconnect(); }

  _resize() {
    const parent = this.canvas.parentElement ?? this.canvas;
    const rect = parent.getBoundingClientRect();
    // Render at device pixel density; a 1x canvas on a retina display looks
    // like a screenshot of a chart rather than a chart.
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    this.w = Math.max(1, Math.floor(rect.width));
    this.h = Math.max(1, Math.floor(rect.height));
    this.canvas.width = this.w * dpr;
    this.canvas.height = this.h * dpr;
    this.canvas.style.width = `${this.w}px`;
    this.canvas.style.height = `${this.h}px`;
    this.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    this.draw();
  }

  /** @param {{key:string,label:string,color:string,points:{t:string,v:number}[]}[]} series */
  setSeries(series) { this.series = series ?? []; this.draw(); }

  draw() {
    const { ctx, w, h } = this;
    if (!ctx || !w || !h) return;
    ctx.clearRect(0, 0, w, h);

    const pad = { top: 14, right: 10, bottom: 22, left: 46 };
    const plotW = w - pad.left - pad.right;
    const plotH = h - pad.top - pad.bottom;
    if (plotW <= 0 || plotH <= 0) return;

    const active = this.series.filter((s) => s.points && s.points.length > 1);
    if (!active.length) {
      ctx.fillStyle = cssVar('text-3');
      ctx.font = '12px system-ui, sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText('waiting for the first control-loop samples…', w / 2, h / 2);
      return;
    }

    const n = Math.min(...active.map((s) => s.points.length));
    const totals = new Array(n).fill(0);
    for (const s of active) {
      const off = s.points.length - n;
      for (let i = 0; i < n; i++) totals[i] += Math.max(0, s.points[off + i].v);
    }
    const peak = Math.max(1, ...totals) * 1.15;

    const x = (i) => pad.left + (i / (n - 1)) * plotW;
    const y = (v) => pad.top + plotH - (v / peak) * plotH;

    // Gridlines and y labels.
    ctx.strokeStyle = cssVar('border');
    ctx.fillStyle = cssVar('text-3');
    ctx.font = '10px ui-monospace, monospace';
    ctx.textAlign = 'right';
    ctx.lineWidth = 1;
    for (let g = 0; g <= 4; g++) {
      const v = (peak / 4) * g;
      const yy = Math.round(y(v)) + 0.5;
      ctx.globalAlpha = g === 0 ? 0.9 : 0.35;
      ctx.beginPath();
      ctx.moveTo(pad.left, yy);
      ctx.lineTo(w - pad.right, yy);
      ctx.stroke();
      ctx.globalAlpha = 1;
      ctx.fillText(v >= 100 ? v.toFixed(0) : v.toFixed(1), pad.left - 8, yy + 3);
    }

    // Stacked areas, drawn bottom-up.
    const baseline = new Array(n).fill(0);
    for (const s of active) {
      const off = s.points.length - n;
      ctx.beginPath();
      for (let i = 0; i < n; i++) {
        const top = baseline[i] + Math.max(0, s.points[off + i].v);
        const px = x(i);
        const py = y(top);
        if (i === 0) ctx.moveTo(px, py); else ctx.lineTo(px, py);
      }
      for (let i = n - 1; i >= 0; i--) ctx.lineTo(x(i), y(baseline[i]));
      ctx.closePath();
      ctx.fillStyle = s.color;
      ctx.globalAlpha = 0.26;
      ctx.fill();
      ctx.globalAlpha = 1;

      ctx.beginPath();
      for (let i = 0; i < n; i++) {
        const top = baseline[i] + Math.max(0, s.points[off + i].v);
        const px = x(i);
        const py = y(top);
        if (i === 0) ctx.moveTo(px, py); else ctx.lineTo(px, py);
      }
      ctx.strokeStyle = s.color;
      ctx.lineWidth = 1.6;
      ctx.lineJoin = 'round';
      ctx.stroke();

      for (let i = 0; i < n; i++) baseline[i] += Math.max(0, s.points[off + i].v);
    }

    // Time axis: first, middle and last sample.
    const first = active[0].points[active[0].points.length - n];
    const last = active[0].points[active[0].points.length - 1];
    ctx.fillStyle = cssVar('text-3');
    ctx.font = '10px ui-monospace, monospace';
    ctx.textAlign = 'left';
    ctx.fillText(shortTime(first?.t), pad.left, h - 6);
    ctx.textAlign = 'right';
    ctx.fillText(shortTime(last?.t), w - pad.right, h - 6);

    // Hover crosshair with a per-series readout.
    if (this.hover && this.hover.x >= pad.left && this.hover.x <= w - pad.right) {
      const i = Math.round(((this.hover.x - pad.left) / plotW) * (n - 1));
      const px = Math.round(x(i)) + 0.5;
      ctx.strokeStyle = cssVar('border-hi');
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(px, pad.top);
      ctx.lineTo(px, pad.top + plotH);
      ctx.stroke();

      const rows = active.map((s) => {
        const off = s.points.length - n;
        return { label: s.label, color: s.color, v: s.points[off + i]?.v ?? 0 };
      }).reverse();

      const bw = 152;
      const bh = 20 + rows.length * 16;
      const bx = Math.min(px + 12, w - bw - 6);
      const by = Math.max(pad.top, Math.min(this.hover.y - bh / 2, h - bh - 6));

      ctx.fillStyle = cssVar('surface-2');
      ctx.strokeStyle = cssVar('border-hi');
      roundRect(ctx, bx, by, bw, bh, 8);
      ctx.fill();
      ctx.stroke();

      ctx.font = '10px ui-monospace, monospace';
      ctx.textAlign = 'left';
      ctx.fillStyle = cssVar('text-3');
      ctx.fillText(shortTime(active[0].points[active[0].points.length - n + i]?.t), bx + 10, by + 14);
      rows.forEach((r, k) => {
        const ry = by + 30 + k * 16;
        ctx.fillStyle = r.color;
        ctx.fillRect(bx + 10, ry - 7, 7, 7);
        ctx.fillStyle = cssVar('text-2');
        ctx.fillText(r.label, bx + 23, ry);
        ctx.textAlign = 'right';
        ctx.fillStyle = cssVar('text');
        ctx.fillText(r.v.toFixed(1), bx + bw - 10, ry);
        ctx.textAlign = 'left';
      });
    }
  }
}

function shortTime(t) {
  if (!t) return '';
  const d = new Date(t);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleTimeString(undefined, { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

function roundRect(ctx, x, y, w, h, r) {
  ctx.beginPath();
  ctx.moveTo(x + r, y);
  ctx.arcTo(x + w, y, x + w, y + h, r);
  ctx.arcTo(x + w, y + h, x, y + h, r);
  ctx.arcTo(x, y + h, x, y, r);
  ctx.arcTo(x, y, x + w, y, r);
  ctx.closePath();
}

/* ── Radar ─────────────────────────────────────────────────────────────── */

/**
 * A radar comparing providers across the four routing objectives.
 *
 * Every axis is normalised so the outer ring is the fleet's worst observed
 * value and the centre is its best. A larger polygon is therefore a worse
 * provider on that axis, which is the opposite of the usual radar convention,
 * so the axis labels say "lower is better" and the legend repeats it.
 */
export function radar(container, groups, axes) {
  const size = 260;
  const cx = size / 2;
  const cy = size / 2;
  const R = size / 2 - 42;
  const k = axes.length;

  if (!groups.length || !k) {
    container.innerHTML = '<div class="empty"><p>No comparable signals yet.</p></div>';
    return;
  }

  const angle = (i) => (Math.PI * 2 * i) / k - Math.PI / 2;
  const pt = (i, r) => [cx + Math.cos(angle(i)) * R * r, cy + Math.sin(angle(i)) * R * r];

  let svg = `<svg viewBox="0 0 ${size} ${size}" role="img"
    aria-label="Radar comparing each cloud across cost, latency, carbon and reliability. Lower is better.">`;

  for (let ring = 1; ring <= 4; ring++) {
    const r = ring / 4;
    const pts = Array.from({ length: k }, (_, i) => pt(i, r).map((n) => n.toFixed(1)).join(',')).join(' ');
    svg += `<polygon points="${pts}" fill="none" stroke="${cssVar('border')}"
      stroke-width="1" opacity="${ring === 4 ? 0.9 : 0.45}"/>`;
  }
  for (let i = 0; i < k; i++) {
    const [x, y] = pt(i, 1);
    svg += `<line x1="${cx}" y1="${cy}" x2="${x.toFixed(1)}" y2="${y.toFixed(1)}"
      stroke="${cssVar('border')}" stroke-width="1" opacity=".55"/>`;
    const [lx, ly] = pt(i, 1.24);
    svg += `<text x="${lx.toFixed(1)}" y="${ly.toFixed(1)}" fill="${cssVar('text-3')}"
      font-size="10" text-anchor="middle" dominant-baseline="middle"
      font-family="ui-monospace, monospace">${esc(axes[i].label)}</text>`;
  }

  for (const g of groups) {
    const pts = g.values.map((v, i) => pt(i, Math.max(0.04, Math.min(1, v))).map((n) => n.toFixed(1)).join(',')).join(' ');
    svg += `<polygon points="${pts}" fill="${esc(g.color)}" fill-opacity=".14"
      stroke="${esc(g.color)}" stroke-width="1.8" stroke-linejoin="round"/>`;
    g.values.forEach((v, i) => {
      const [x, y] = pt(i, Math.max(0.04, Math.min(1, v)));
      svg += `<circle cx="${x.toFixed(1)}" cy="${y.toFixed(1)}" r="2.4" fill="${esc(g.color)}"/>`;
    });
  }
  svg += '</svg>';

  const legend = groups.map((g) =>
    `<span class="legend__item"><span class="legend__swatch" style="background:${esc(g.color)}"></span>${esc(g.label)}</span>`
  ).join('');

  container.innerHTML = `${svg}<div class="legend">${legend}</div>
    <p class="card__sub" style="text-align:center">Outer ring is the fleet's worst value on that axis — a smaller shape is a better provider.</p>`;
}

/* ── Score contribution bar ────────────────────────────────────────────── */

const DIMS = [
  { key: 'cost', label: 'cost', varName: 'cost' },
  { key: 'latency', label: 'latency', varName: 'latency' },
  { key: 'carbon', label: 'carbon', varName: 'carbon' },
  { key: 'reliability', label: 'reliability', varName: 'reliability' },
];

/**
 * Render one candidate's score derivation as a stacked penalty bar.
 *
 * Each segment is that dimension's normalised value multiplied by its
 * objective weight, so the segments sum to the total penalty and the empty
 * remainder of the track is the score. This is the arithmetic the router
 * actually performed, not a re-derivation.
 */
export function contributionBar(candidate) {
  const contrib = candidate.contribution ?? {};
  const norm = candidate.normalized ?? {};
  const raw = candidate.raw ?? {};

  const segs = DIMS.map((d) => ({
    ...d,
    value: Math.max(0, contrib[d.key] ?? 0),
    norm: norm[d.key] ?? 0,
    raw: raw[d.key] ?? 0,
    color: cssVar(d.varName),
  }));

  const bar = segs.map((s) =>
    `<span class="cand__seg" style="width:${(s.value * 100).toFixed(2)}%;background:${esc(s.color)}"
       title="${esc(s.label)} penalty ${(s.value * 100).toFixed(1)}%"></span>`
  ).join('');

  const legend = segs.map((s) =>
    `<span class="legend__item">
       <span class="legend__swatch" style="background:${esc(s.color)}"></span>
       ${esc(s.label)} <b>${formatRaw(s.key, s.raw)}</b>
     </span>`
  ).join('');

  return { bar, legend };
}

function formatRaw(key, v) {
  switch (key) {
    case 'cost': return `$${v.toFixed(4)}/GB`;
    case 'latency': return ms(v);
    case 'carbon': return `${v.toFixed(2)} g/GB`;
    case 'reliability': return `${(v * 100).toFixed(2)}% err`;
    default: return num(v, 2);
  }
}

/* ── Allocation bars ───────────────────────────────────────────────────── */

/** Render a route's traffic distribution as labelled bars. */
export function allocationBars(candidates, { rps = 0 } = {}) {
  if (!candidates?.length) return '<div class="empty"><p>No candidates in this route.</p></div>';

  const rows = candidates.map((c) => {
    const shed = !c.eligible || c.weight <= 0;
    const color = cloudColor(c.cloud);
    const share = Math.max(0, c.weight ?? 0);
    return `<div class="alloc__row ${shed ? 'is-shed' : ''}">
      <span class="alloc__name">
        <span class="cloud-dot cloud-${esc(c.cloud)}" aria-hidden="true"></span>
        <span title="${esc(c.backendId)}">${esc(c.backendId)}</span>
      </span>
      <span class="alloc__track">
        <span class="alloc__fill" style="width:${(share * 100).toFixed(2)}%;background:${esc(color)}"></span>
      </span>
      <span class="alloc__pct">${(share * 100).toFixed(1)}%</span>
      ${shed && c.reason ? `<span class="alloc__reason">${esc(c.reason)}</span>` : ''}
    </div>`;
  }).join('');

  return `<div class="alloc">
    <div class="alloc__head">
      <span>${candidates.filter((c) => c.weight > 0).length} of ${candidates.length} carrying traffic</span>
      <span>${num(rps, 1)} req/s</span>
    </div>
    ${rows}
  </div>`;
}

/* ── Ribbon ────────────────────────────────────────────────────────────── */

/** A single stacked bar of provider share, used in the KPI strip. */
export function shareRibbon(clouds) {
  const total = clouds.reduce((a, c) => a + Math.max(0, c.share), 0) || 1;
  return clouds.map((c) => {
    const w = (Math.max(0, c.share) / total) * 100;
    if (w < 0.4) return '';
    return `<span class="alloc__fill" style="width:${w.toFixed(2)}%;background:${esc(cloudColor(c.cloud))}"
      title="${esc(cloudLabel(c.cloud))} ${w.toFixed(1)}%"></span>`;
  }).join('');
}

export { reducedMotion, usd };
