// Memory detail: content, provenance, the bi-temporal version timeline,
// lifecycle history and actions (correct, invalidate, delete).
import { call, h, page, table, short, status, badge, pre, date, guard, toast } from '../lib.js';

const NS = 'http://www.w3.org/2000/svg';
const svg = (tag, attrs, ...kids) => {
  const el = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  kids.forEach((k) => el.append(k));
  return el;
};

// timeline draws one bar per version: valid_from → valid_to (or now),
// red marker where a version was invalidated (a correction).
function timeline(versions) {
  const now = Date.now();
  const t = (s) => (s ? new Date(s).getTime() : now);
  const min = Math.min(...versions.map((v) => t(v.valid_from), now));
  const max = Math.max(now, ...versions.map((v) => t(v.valid_to)));
  const W = 760, X0 = 150, row = 26, span = Math.max(max - min, 1);
  const x = (ms) => X0 + ((ms - min) / span) * (W - X0 - 10);
  const s = svg('svg', { width: '100%', viewBox: `0 0 ${W} ${versions.length * row + 24}` });
  versions.forEach((v, i) => {
    const y = i * row + 6;
    const end = v.valid_to ? t(v.valid_to) : now;
    const color = v.invalidated_at ? '#f7768e' : v.valid_to ? '#e0af68' : '#9ece6a';
    s.append(svg('text', { x: 0, y: y + 13 }, `v${v.version} ${short(v.id)}`));
    s.append(svg('rect', { x: x(t(v.valid_from)), y, width: Math.max(x(end) - x(t(v.valid_from)), 3), height: 16, rx: 3, fill: color, opacity: 0.85 }));
    if (v.invalidated_at) s.append(svg('line', { x1: x(t(v.invalidated_at)), x2: x(t(v.invalidated_at)), y1: y - 2, y2: y + 18, stroke: '#f7768e', 'stroke-width': 2 }));
  });
  s.append(svg('text', { x: X0, y: versions.length * row + 20 }, date(min)));
  s.append(svg('text', { x: W - 150, y: versions.length * row + 20 }, 'now ' + date(now)));
  return s;
}

export default async function (root, [id]) {
  const m = await call('GET', '/v1/memories/' + id);
  const [{ versions }, { events }] = await Promise.all([
    call('GET', `/v1/memories/${id}/versions`), call('GET', `/v1/memories/${id}/history`)]);
  const edit = h('textarea', { rows: 3 });
  edit.value = m.text;
  const reload = () => (location.hash = '#/memory/' + id + '?' + Date.now());
  const correct = guard(async () => {
    const n = await call('PATCH', '/v1/memories/' + id, { text: edit.value, reason: 'corrected in dev console' });
    toast('New version v' + n.version);
    location.hash = '#/memory/' + n.id;
  });
  const invalidate = guard(async () => { await call('POST', `/v1/memories/${id}/invalidate`, { reason: 'dev console' }); toast('Invalidated'); reload(); });
  const del = guard(async () => {
    if (!confirm('Delete permanently? History keeps only the audit trail.')) return;
    await call('DELETE', '/v1/memories/' + id); toast('Deleted'); location.hash = '#/memories';
  });
  const src = m.source || {};
  page(root, m.title || 'Memory ' + short(m.id), null,
    h('div', { class: 'grid2' },
      h('div', { class: 'card' },
        h('p', null, status(m), badge(m.kind), badge('L' + m.layer), badge(m.visibility), badge('trust ' + src.trust, src.trust === 'low' ? 'warn' : ''),
          (src.flags || []).map((f) => badge(f, 'bad'))),
        h('h2', null, m.text),
        h('div', { class: 'kv' },
          h('div', null, 'id'), h('div', { class: 'mono' }, m.id),
          h('div', null, 'owner'), h('div', null, [m.tenant, m.user_id, m.agent_id, m.run_id].filter(Boolean).join(' / ')),
          h('div', null, 'relation'), h('div', null, [m.subject, m.predicate, m.object].filter(Boolean).join(' → ') || '—'),
          h('div', null, 'slot'), h('div', { class: 'mono' }, m.slot || '—'),
          h('div', null, 'valid'), h('div', null, date(m.valid_from), ' → ', m.valid_to ? date(m.valid_to) : 'open'),
          h('div', null, 'recorded'), h('div', null, date(m.recorded_at), m.invalidated_at ? ' · invalidated ' + date(m.invalidated_at) : ''),
          h('div', null, 'expires'), h('div', null, m.expires_at ? date(m.expires_at) : '—'),
          h('div', null, 'confidence / importance'), h('div', null, `${m.confidence} / ${m.importance} · heat ${m.heat}`),
          h('div', null, 'tags'), h('div', null, (m.tags || []).map((t) => badge(t))),
          h('div', null, 'entities'), h('div', null, (m.entities || []).map((e) => h('a', { href: '#/graph/' + encodeURIComponent(e.name) }, badge(e.name + (e.type ? ' · ' + e.type : ''))))),
          h('div', null, 'source'), h('div', null, `${src.kind}${src.actor ? ' by ' + src.actor : ''}${src.role ? ' (' + src.role + ')' : ''}`, h('div', { class: 'mono muted' }, src.derived_by || '')),
          h('div', null, 'evidence'), h('div', null, (src.evidence || []).map((e) =>
            h('div', { class: 'mono' }, h('a', { href: '#/conversations/' + encodeURIComponent(m.run_id || '') + '/' + e.episode }, 'turn ' + short(e.episode)), ` part ${e.part} bytes ${e.start}–${e.end}`))),
          h('div', null, 'metadata'), h('div', null, m.metadata ? pre(m.metadata) : '—')),
        h('h2', null, 'Correct (new version)'), edit,
        h('div', { class: 'row' },
          h('button', { class: 'primary', onclick: correct }, 'Save correction'),
          h('button', { onclick: invalidate }, 'Invalidate (ended now)'),
          h('button', { class: 'danger', onclick: del }, 'Delete'))),
      h('div', null,
        h('div', { class: 'card' }, h('h2', null, 'Version timeline (valid time)'), timeline(versions),
          h('div', { class: 'legend' }, h('span', null, h('i', { class: 'ent' }), 'current'), h('span', null, h('i', { class: 'prior' }), 'closed (world changed)'), h('span', null, '🟥 invalidated (correction)')),
          table([
            { label: 'v', key: 'version' },
            { label: 'text', render: (v) => (v.id === m.id ? h('b', null, v.text) : v.text) },
            { label: 'state', render: status },
          ], versions, (v) => (location.hash = '#/memory/' + v.id))),
        h('div', { class: 'card' }, h('h2', null, 'History'),
          table([
            { label: 'at', render: (e) => date(e.at) }, { label: 'action', render: (e) => badge(e.action) },
            { label: 'change', render: (e) => [e.old && h('div', { class: 'muted' }, '− ' + e.old), e.new && h('div', null, '+ ' + e.new)] },
            { label: 'by', key: 'actor' }, { label: 'reason', key: 'reason' },
          ], events)))));
}
