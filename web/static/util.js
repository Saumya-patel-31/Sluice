/* Formatting, escaping and icon primitives shared by every view. */

/* ── Escaping ──────────────────────────────────────────────────────────── */

/** Escape a value for interpolation into innerHTML. */
export function esc(v) {
  return String(v ?? '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}

/* ── Numbers ───────────────────────────────────────────────────────────── */

/**
 * Money, scaled to stay legible across nine orders of magnitude.
 *
 * The same formatter renders a cumulative figure in the thousands and a
 * single request's egress delta, which on an 8 KB response is a few
 * hundred-billionths of a dollar. Scientific notation is the wrong answer for
 * both — "$1.1e-8" tells an operator nothing — so very small values switch to
 * micro-dollars, and values below a tenth of a micro-dollar are reported as
 * effectively zero rather than given false precision.
 */
export function usd(v, { sign = false } = {}) {
  if (v == null || Number.isNaN(v)) return '—';
  const a = Math.abs(v);
  const s = sign && v > 0 ? '+' : v < 0 ? '−' : '';
  if (a === 0) return '$0';
  if (a >= 1e6) return `${s}$${(a / 1e6).toFixed(2)}M`;
  if (a >= 1e3) return `${s}$${(a / 1e3).toFixed(2)}k`;
  if (a >= 1) return `${s}$${a.toFixed(2)}`;
  if (a >= 0.01) return `${s}$${a.toFixed(3)}`;
  if (a >= 0.0001) return `${s}$${a.toFixed(5)}`;
  if (a >= 1e-7) return `${s}${(a * 1e6).toFixed(a * 1e6 >= 10 ? 0 : 1)} µ$`;
  return '≈$0';
}

/** Mass of CO2e, scaled from milligrams to tonnes. */
export function grams(v, { sign = false } = {}) {
  if (v == null || Number.isNaN(v)) return '—';
  const a = Math.abs(v);
  const s = sign && v > 0 ? '+' : v < 0 ? '−' : '';
  if (a === 0) return '0 g';
  if (a >= 1e6) return `${s}${(a / 1e6).toFixed(2)} t`;
  if (a >= 1e3) return `${s}${(a / 1e3).toFixed(2)} kg`;
  if (a >= 1) return `${s}${a.toFixed(1)} g`;
  if (a >= 0.001) return `${s}${(a * 1000).toFixed(a * 1000 >= 10 ? 0 : 1)} mg`;
  return '≈0 g';
}

/** A count with thousands separators and bounded precision. */
export function num(v, digits = 0) {
  if (v == null || Number.isNaN(v)) return '—';
  return v.toLocaleString(undefined, {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
}

/** Compact count: 1.2k, 3.4M. */
export function compact(v) {
  if (v == null || Number.isNaN(v)) return '—';
  const a = Math.abs(v);
  if (a >= 1e9) return `${(v / 1e9).toFixed(1)}B`;
  if (a >= 1e6) return `${(v / 1e6).toFixed(1)}M`;
  if (a >= 1e3) return `${(v / 1e3).toFixed(1)}k`;
  return num(v, a < 10 && a % 1 !== 0 ? 1 : 0);
}

/** Milliseconds with precision that shrinks as the value grows. */
export function ms(v) {
  if (v == null || Number.isNaN(v)) return '—';
  if (v >= 1000) return `${(v / 1000).toFixed(2)}s`;
  if (v >= 100) return `${v.toFixed(0)}ms`;
  if (v >= 10) return `${v.toFixed(1)}ms`;
  return `${v.toFixed(2)}ms`;
}

/** A ratio in [0,1] as a percentage. */
export function pct(v, digits = 1) {
  if (v == null || Number.isNaN(v)) return '—';
  return `${(v * 100).toFixed(digits)}%`;
}

/** Bytes in binary units. */
export function bytes(v) {
  if (v == null || Number.isNaN(v)) return '—';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = 0;
  let x = v;
  while (x >= 1024 && i < units.length - 1) { x /= 1024; i++; }
  return `${x.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

/** A duration in seconds as a coarse human span. */
export function duration(seconds) {
  if (seconds == null || Number.isNaN(seconds)) return '—';
  const s = Math.floor(seconds);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

/** Wall-clock time of day. */
export function clock(iso) {
  const d = iso instanceof Date ? iso : new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString(undefined, { hour12: false });
}

/** Time of day with milliseconds, for the decision feed. */
export function clockMs(iso) {
  const d = iso instanceof Date ? iso : new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return `${d.toLocaleTimeString(undefined, { hour12: false })}.${String(d.getMilliseconds()).padStart(3, '0')}`;
}

/* ── Cloud identity ────────────────────────────────────────────────────── */

const CLOUD_LABELS = { aws: 'AWS', gcp: 'Google Cloud', azure: 'Azure', onprem: 'On-prem' };

export function cloudLabel(c) { return CLOUD_LABELS[c] ?? c ?? 'unknown'; }

/** Resolve a provider's accent from the stylesheet, so colour lives in CSS. */
export function cloudColor(c) {
  const v = getComputedStyle(document.documentElement).getPropertyValue(`--${c}`).trim();
  return v || '#7A8AA3';
}

export function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(`--${name}`).trim();
}

/** A small square swatch identifying a provider. */
export function cloudDot(c) {
  return `<span class="cloud-dot cloud-${esc(c)}" aria-hidden="true"></span>`;
}

/* ── Verdicts ──────────────────────────────────────────────────────────── */

/**
 * Render a verdict badge.
 *
 * The verdict word is always present, never colour alone — an operator with
 * a colour-vision deficiency has to be able to read an audit trail.
 */
export function verdictBadge(v) {
  const map = {
    allow: ['badge--allow', 'allowed'],
    deny: ['badge--deny', 'denied'],
    no_capacity: ['badge--nocap', 'no capacity'],
  };
  const [cls, label] = map[v] ?? ['badge--mute', v ?? 'unknown'];
  return `<span class="badge ${cls}">${esc(label)}</span>`;
}

/* ── Icons ─────────────────────────────────────────────────────────────── */

/* Hand-drawn on a 24x24 grid so every glyph shares stroke weight and optical
   size. No emoji anywhere in the interface: they render differently on every
   platform and carry connotations nobody chose. */
const ICONS = {
  gauge: '<path d="M12 14a2 2 0 1 0 0-4 2 2 0 0 0 0 4Z"/><path d="m13.4 10.6 4.2-4.2"/><path d="M3.6 18a9 9 0 1 1 16.8 0"/>',
  network: '<rect x="9" y="2" width="6" height="6" rx="1.5"/><rect x="2" y="16" width="6" height="6" rx="1.5"/><rect x="16" y="16" width="6" height="6" rx="1.5"/><path d="M12 8v4M5 16v-2h14v2"/>',
  list: '<path d="M8 6h13M8 12h13M8 18h13M3.5 6h.01M3.5 12h.01M3.5 18h.01"/>',
  shield: '<path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10Z"/><path d="m9 12 2 2 4-4"/>',
  activity: '<path d="M3 12h4l3-8 4 16 3-8h4"/>',
  pointer: '<path d="m4 4 7 16 2.5-6.5L20 11 4 4Z"/>',
  flask: '<path d="M9 2v6L4 19a2 2 0 0 0 1.8 3h12.4A2 2 0 0 0 20 19L15 8V2"/><path d="M8 2h8M6.5 14h11"/>',
  zap: '<path d="M13 2 4 14h7l-1 8 9-12h-7l1-8Z"/>',
  alert: '<path d="M12 9v4M12 17h.01"/><path d="M10.3 3.9 2.4 17a2 2 0 0 0 1.7 3h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z"/>',
  check: '<path d="m5 12 5 5L20 7"/>',
  leaf: '<path d="M11 20A7 7 0 0 1 4 13c0-6 5-9 16-10 0 10-4 16-9 16Z"/><path d="M4 21c2-6 6-9 10-11"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
};

/** Return an SVG icon by name, or an empty string if unknown. */
export function icon(name, extraClass = '') {
  const body = ICONS[name];
  if (!body) return '';
  return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"
    stroke-linecap="round" stroke-linejoin="round" class="${esc(extraClass)}"
    aria-hidden="true" focusable="false">${body}</svg>`;
}

/** Replace every [data-icon] placeholder in a root element with its glyph. */
export function hydrateIcons(root = document) {
  root.querySelectorAll('[data-icon]').forEach((node) => {
    if (node.dataset.iconDone === '1') return;
    node.innerHTML = icon(node.dataset.icon);
    node.dataset.iconDone = '1';
  });
}

/* ── Misc ──────────────────────────────────────────────────────────────── */

export const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

/** Trailing-edge throttle, used to keep re-renders off the animation frame. */
export function throttle(fn, wait) {
  let last = 0;
  let timer = null;
  let pending = null;
  return (...args) => {
    pending = args;
    const now = Date.now();
    const wait_ = Math.max(0, wait - (now - last));
    if (wait_ === 0) {
      last = now;
      fn(...pending);
      return;
    }
    if (timer) return;
    timer = setTimeout(() => {
      timer = null;
      last = Date.now();
      fn(...pending);
    }, wait_);
  };
}

/** True when the viewer has asked for reduced motion. */
export function reducedMotion() {
  return window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}
