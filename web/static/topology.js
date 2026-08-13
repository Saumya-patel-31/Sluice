/* The topology view: gateway → cloud → region, with traffic drawn as motion.
 *
 * The layout is computed deterministically rather than by force simulation.
 * An operator watching a routing graph needs regions to stay where they were
 * a second ago; a settling simulation makes the picture unreadable exactly
 * when it matters, which is while traffic is shifting.
 */

import { cssVar, cloudColor, cloudLabel, esc, ms, num, pct, reducedMotion } from './util.js';

const OVERLAYS = {
  traffic: { label: 'Traffic', metric: (n) => n.share, format: (n) => pct(n.share, 1), invert: false },
  carbon:  { label: 'Carbon',  metric: (n) => n.carbonGramsPerGb, format: (n) => `${(n.carbonGramsPerGb ?? 0).toFixed(2)} g/GB`, invert: true },
  cost:    { label: 'Cost',    metric: (n) => n.egressUsdPerGb,   format: (n) => `$${(n.egressUsdPerGb ?? 0).toFixed(4)}/GB`, invert: true },
  latency: { label: 'Latency', metric: (n) => n.latencyP95Ms,     format: (n) => ms(n.latencyP95Ms), invert: true },
};

export class TopologyView {
  constructor(canvas, tooltip) {
    this.canvas = canvas;
    this.tooltip = tooltip;
    this.ctx = canvas.getContext('2d');
    this.data = { nodes: [], edges: [] };
    this.overlay = 'traffic';
    this.layout = new Map();
    this.particles = [];
    this.hover = null;
    this.pointer = null;
    this.running = false;
    this.lastFrame = 0;

    this._frame = this._frame.bind(this);
    this._resize = this._resize.bind(this);

    this._observer = new ResizeObserver(this._resize);
    this._observer.observe(canvas.parentElement ?? canvas);

    canvas.addEventListener('pointermove', (e) => {
      const r = canvas.getBoundingClientRect();
      this.pointer = { x: e.clientX - r.left, y: e.clientY - r.top };
      this._hitTest();
    });
    canvas.addEventListener('pointerleave', () => {
      this.pointer = null;
      this.hover = null;
      this.tooltip.hidden = true;
      this.draw();
    });

    document.addEventListener('visibilitychange', () => {
      if (document.hidden) this.stop(); else if (this.data.nodes.length) this.start();
    });

    this._resize();
  }

  destroy() { this.stop(); this._observer.disconnect(); }

  setOverlay(name) {
    if (OVERLAYS[name]) { this.overlay = name; this.draw(); }
  }

  setData(data) {
    this.data = data ?? { nodes: [], edges: [] };
    this._computeLayout();
    this._syncParticles();
    if (this.data.nodes.length && !document.hidden) this.start();
    this.draw();
  }

  start() {
    if (this.running) return;
    // With reduced motion the graph is still drawn, just not animated; the
    // information is in the layout and the edge weights, not in the movement.
    if (reducedMotion()) { this.draw(); return; }
    this.running = true;
    this.lastFrame = performance.now();
    requestAnimationFrame(this._frame);
  }

  stop() { this.running = false; }

  _resize() {
    const parent = this.canvas.parentElement ?? this.canvas;
    const rect = parent.getBoundingClientRect();
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    this.w = Math.max(1, Math.floor(rect.width));
    this.h = Math.max(1, Math.floor(rect.height));
    this.canvas.width = this.w * dpr;
    this.canvas.height = this.h * dpr;
    this.canvas.style.width = `${this.w}px`;
    this.canvas.style.height = `${this.h}px`;
    this.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    this._computeLayout();
    this.draw();
  }

  /**
   * Place nodes in three deterministic columns.
   *
   * Regions are grouped under their provider and ordered by ID so a region
   * never swaps rows between frames. Vertical space is divided among all
   * regions rather than per provider, which keeps spacing even when one cloud
   * has six regions and another has two.
   */
  _computeLayout() {
    this.layout.clear();
    const { w, h } = this;
    if (!w || !h) return;

    const nodes = this.data.nodes ?? [];
    const gateway = nodes.find((n) => n.kind === 'gateway');
    const clouds = nodes.filter((n) => n.kind === 'cloud').sort((a, b) => a.id.localeCompare(b.id));
    const regions = nodes.filter((n) => n.kind === 'region');

    const xGate = Math.max(76, w * 0.09);
    const xCloud = w * 0.38;
    const xRegion = Math.min(w - 130, w * 0.78);

    if (gateway) this.layout.set(gateway.id, { x: xGate, y: h / 2, r: 26 });

    const byCloud = new Map();
    for (const c of clouds) byCloud.set(c.id, []);
    for (const r of regions) {
      if (!byCloud.has(r.cloud)) byCloud.set(r.cloud, []);
      byCloud.get(r.cloud).push(r);
    }
    for (const list of byCloud.values()) list.sort((a, b) => a.id.localeCompare(b.id));

    const totalRegions = regions.length || 1;
    const topPad = 46;
    const usable = Math.max(60, h - topPad * 2);
    const gap = usable / totalRegions;

    let row = 0;
    for (const c of clouds) {
      const list = byCloud.get(c.id) ?? [];
      const firstRow = row;
      for (const r of list) {
        const y = topPad + gap * (row + 0.5);
        this.layout.set(r.id, { x: xRegion, y, r: 9 + Math.sqrt(Math.max(0, r.share)) * 15 });
        row++;
      }
      const midRow = list.length ? (firstRow + row - 1) / 2 : row;
      this.layout.set(c.id, { x: xCloud, y: topPad + gap * (midRow + 0.5), r: 18 });
    }
  }

  /** Keep one particle stream per active edge, sized by its traffic share. */
  _syncParticles() {
    this.particles = [];
    for (const e of this.data.edges ?? []) {
      if (!e.active || e.share <= 0.005) continue;
      const count = Math.max(1, Math.round(e.share * 14));
      for (let i = 0; i < count; i++) {
        this.particles.push({ edge: e, t: Math.random(), speed: 0.18 + e.share * 0.5 });
      }
    }
  }

  _frame(now) {
    if (!this.running) return;
    const dt = Math.min(0.1, (now - this.lastFrame) / 1000);
    this.lastFrame = now;
    for (const p of this.particles) {
      p.t += p.speed * dt;
      if (p.t > 1) p.t -= 1;
    }
    this.draw();
    requestAnimationFrame(this._frame);
  }

  _nodeColor(n) {
    if (n.kind === 'gateway') return cssVar('ok');
    if (n.kind === 'cloud') return cloudColor(n.cloud);
    if (!n.healthy) return cssVar('bad');

    const ov = OVERLAYS[this.overlay];
    if (this.overlay === 'traffic') return cloudColor(n.cloud);

    // Non-traffic overlays map the metric onto a green-to-red ramp across the
    // fleet's observed range, so "best available right now" is always green
    // even when the absolute numbers are all poor.
    const regions = this.data.nodes.filter((x) => x.kind === 'region');
    const values = regions.map(ov.metric).filter((v) => typeof v === 'number' && !Number.isNaN(v));
    if (!values.length) return cssVar('text-3');
    const lo = Math.min(...values);
    const hi = Math.max(...values);
    const v = ov.metric(n) ?? lo;
    let t = hi - lo < 1e-9 ? 0 : (v - lo) / (hi - lo);
    if (!ov.invert) t = 1 - t;
    return mixHex(cssVar('ok'), cssVar('bad'), t);
  }

  draw() {
    const { ctx, w, h } = this;
    if (!ctx || !w || !h) return;
    ctx.clearRect(0, 0, w, h);

    const nodes = this.data.nodes ?? [];
    if (!nodes.length) {
      ctx.fillStyle = cssVar('text-3');
      ctx.font = '13px system-ui, sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText('waiting for a traffic plan…', w / 2, h / 2);
      return;
    }
    const byId = new Map(nodes.map((n) => [n.id, n]));

    // Edges.
    for (const e of this.data.edges ?? []) {
      const a = this.layout.get(e.from);
      const b = this.layout.get(e.to);
      if (!a || !b) continue;
      const target = byId.get(e.to);
      const active = e.active && e.share > 0.001;

      ctx.beginPath();
      curve(ctx, a, b);
      ctx.strokeStyle = active ? this._nodeColor(target ?? {}) : cssVar('border');
      ctx.globalAlpha = active ? 0.16 + Math.min(0.5, e.share * 1.1) : 0.16;
      ctx.lineWidth = active ? 1.2 + e.share * 7 : 1;
      if (!active) ctx.setLineDash([3, 5]);
      ctx.stroke();
      ctx.setLineDash([]);
      ctx.globalAlpha = 1;
    }

    // Particles.
    for (const p of this.particles) {
      const a = this.layout.get(p.edge.from);
      const b = this.layout.get(p.edge.to);
      if (!a || !b) continue;
      const [x, y] = curvePoint(a, b, p.t);
      const target = byId.get(p.edge.to);
      ctx.beginPath();
      ctx.arc(x, y, 1.9, 0, Math.PI * 2);
      ctx.fillStyle = this._nodeColor(target ?? {});
      ctx.globalAlpha = 0.85;
      ctx.fill();
      ctx.globalAlpha = 1;
    }

    // Nodes.
    for (const n of nodes) {
      const pos = this.layout.get(n.id);
      if (!pos) continue;
      const color = this._nodeColor(n);
      const isHover = this.hover?.id === n.id;

      if (n.share > 0.01) {
        ctx.beginPath();
        ctx.arc(pos.x, pos.y, pos.r + 7 + n.share * 8, 0, Math.PI * 2);
        ctx.fillStyle = color;
        ctx.globalAlpha = 0.09;
        ctx.fill();
        ctx.globalAlpha = 1;
      }

      ctx.beginPath();
      ctx.arc(pos.x, pos.y, pos.r, 0, Math.PI * 2);
      ctx.fillStyle = cssVar('surface');
      ctx.fill();
      ctx.lineWidth = isHover ? 3 : 2;
      ctx.strokeStyle = color;
      ctx.globalAlpha = n.kind === 'region' && !n.healthy ? 0.6 : 1;
      ctx.stroke();
      ctx.globalAlpha = 1;

      // An ejected region gets a cross as well as a colour change, so the
      // state survives a greyscale screenshot in an incident review.
      if (n.kind === 'region' && !n.healthy) {
        const k = pos.r * 0.45;
        ctx.beginPath();
        ctx.moveTo(pos.x - k, pos.y - k);
        ctx.lineTo(pos.x + k, pos.y + k);
        ctx.moveTo(pos.x + k, pos.y - k);
        ctx.lineTo(pos.x - k, pos.y + k);
        ctx.strokeStyle = cssVar('bad');
        ctx.lineWidth = 2;
        ctx.stroke();
      } else if (n.kind !== 'gateway') {
        ctx.fillStyle = color;
        ctx.font = '600 10px ui-monospace, monospace';
        ctx.textAlign = 'center';
        ctx.textBaseline = 'middle';
        ctx.fillText(`${Math.round((n.share ?? 0) * 100)}`, pos.x, pos.y);
      }

      ctx.fillStyle = isHover ? cssVar('text') : cssVar('text-2');
      ctx.font = n.kind === 'region' ? '11px system-ui, sans-serif' : '600 12px system-ui, sans-serif';
      ctx.textBaseline = 'middle';
      if (n.kind === 'region') {
        ctx.textAlign = 'left';
        ctx.fillText(n.label ?? n.id, pos.x + pos.r + 10, pos.y);
      } else if (n.kind === 'cloud') {
        ctx.textAlign = 'center';
        ctx.fillText(cloudLabel(n.cloud), pos.x, pos.y - pos.r - 12);
      } else {
        ctx.textAlign = 'center';
        ctx.fillText('gateway', pos.x, pos.y + pos.r + 16);
        ctx.fillStyle = cssVar('ok');
        ctx.font = '600 12px ui-monospace, monospace';
        ctx.fillText(`${num(n.rps ?? 0, 0)}/s`, pos.x, pos.y);
      }
    }
    ctx.textBaseline = 'alphabetic';
  }

  _hitTest() {
    if (!this.pointer) return;
    let found = null;
    for (const n of this.data.nodes ?? []) {
      const pos = this.layout.get(n.id);
      if (!pos) continue;
      const dx = this.pointer.x - pos.x;
      const dy = this.pointer.y - pos.y;
      if (dx * dx + dy * dy <= (pos.r + 6) ** 2) { found = n; break; }
    }
    if (found?.id === this.hover?.id) return;
    this.hover = found;
    this._renderTooltip();
    this.draw();
  }

  _renderTooltip() {
    const n = this.hover;
    if (!n) { this.tooltip.hidden = true; return; }
    const pos = this.layout.get(n.id);

    let rows = '';
    if (n.kind === 'region') {
      rows = `
        <div class="topo-tip__row"><span>share</span><b>${pct(n.share ?? 0, 1)}</b></div>
        <div class="topo-tip__row"><span>rate</span><b>${num(n.rps ?? 0, 1)}/s</b></div>
        <div class="topo-tip__row"><span>p95</span><b>${ms(n.latencyP95Ms)}</b></div>
        <div class="topo-tip__row"><span>egress</span><b>$${(n.egressUsdPerGb ?? 0).toFixed(4)}/GB</b></div>
        <div class="topo-tip__row"><span>carbon</span><b>${(n.carbonGramsPerGb ?? 0).toFixed(2)} g/GB</b></div>
        <div class="topo-tip__row"><span>jurisdiction</span><b>${esc(n.jurisdiction || '—')}</b></div>
        <div class="topo-tip__row"><span>grid</span><b>${esc(n.gridZone || '—')}</b></div>
        ${n.reason ? `<div class="topo-tip__row" style="margin-top:6px"><span style="color:var(--warn)">${esc(n.reason)}</span></div>` : ''}`;
    } else if (n.kind === 'cloud') {
      rows = `<div class="topo-tip__row"><span>share</span><b>${pct(n.share ?? 0, 1)}</b></div>
              <div class="topo-tip__row"><span>rate</span><b>${num(n.rps ?? 0, 1)}/s</b></div>`;
    } else {
      rows = `<div class="topo-tip__row"><span>rate</span><b>${num(n.rps ?? 0, 1)}/s</b></div>`;
    }

    this.tooltip.innerHTML = `
      <div class="topo-tip__title">
        ${n.cloud ? `<span class="cloud-dot cloud-${esc(n.cloud)}"></span>` : ''}${esc(n.label ?? n.id)}
      </div>${rows}`;
    this.tooltip.hidden = false;

    // Keep the tooltip inside the canvas rather than letting it clip.
    const tw = this.tooltip.offsetWidth || 200;
    const th = this.tooltip.offsetHeight || 120;
    let x = (pos?.x ?? 0) + 18;
    let y = (pos?.y ?? 0) - th / 2;
    if (x + tw > this.w - 8) x = (pos?.x ?? 0) - tw - 18;
    y = Math.max(8, Math.min(y, this.h - th - 8));
    this.tooltip.style.left = `${x}px`;
    this.tooltip.style.top = `${y}px`;
  }
}

/* ── Geometry ──────────────────────────────────────────────────────────── */

function curve(ctx, a, b) {
  const mx = (a.x + b.x) / 2;
  ctx.moveTo(a.x, a.y);
  ctx.bezierCurveTo(mx, a.y, mx, b.y, b.x, b.y);
}

function curvePoint(a, b, t) {
  const mx = (a.x + b.x) / 2;
  const u = 1 - t;
  const x = u * u * u * a.x + 3 * u * u * t * mx + 3 * u * t * t * mx + t * t * t * b.x;
  const y = u * u * u * a.y + 3 * u * u * t * a.y + 3 * u * t * t * b.y + t * t * t * b.y;
  return [x, y];
}

/* ── Colour ────────────────────────────────────────────────────────────── */

function mixHex(aHex, bHex, t) {
  const a = parseHex(aHex);
  const b = parseHex(bHex);
  if (!a || !b) return aHex;
  const c = a.map((v, i) => Math.round(v + (b[i] - v) * Math.max(0, Math.min(1, t))));
  return `rgb(${c[0]}, ${c[1]}, ${c[2]})`;
}

function parseHex(hex) {
  const m = /^#?([0-9a-f]{6})$/i.exec((hex || '').trim());
  if (!m) return null;
  const n = parseInt(m[1], 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

export { OVERLAYS };
