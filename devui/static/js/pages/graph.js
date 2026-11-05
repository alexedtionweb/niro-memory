// Entity graph: an entity, the memories that mention it, and co-mentioned
// entities, drawn with a small force layout on a canvas.
import { call, h, page, table, short, badge, status, guard } from '../lib.js';

function draw(canvas, center, related, onPick) {
  const W = (canvas.width = canvas.clientWidth * devicePixelRatio);
  const H = (canvas.height = 420 * devicePixelRatio);
  const ctx = canvas.getContext('2d');
  const max = Math.max(1, ...related.map((r) => r.count));
  const nodes = [{ name: center.name, type: center.type, r: 22, x: W / 2, y: H / 2, fixed: true },
    ...related.slice(0, 40).map((r, i) => ({ ...r, r: 8 + 12 * (r.count / max),
      x: W / 2 + Math.cos(i) * 150 * devicePixelRatio, y: H / 2 + Math.sin(i) * 150 * devicePixelRatio, vx: 0, vy: 0 }))];
  // A few hundred iterations of springs (to the center) and repulsion.
  for (let it = 0; it < 300; it++) {
    for (const a of nodes) {
      if (a.fixed) continue;
      const dx = W / 2 - a.x, dy = H / 2 - a.y, d = Math.hypot(dx, dy) || 1;
      const want = (170 - 60 * (a.count / max)) * devicePixelRatio;
      a.vx += (dx / d) * (d - want) * 0.01; a.vy += (dy / d) * (d - want) * 0.01;
      for (const b of nodes) {
        if (a === b) continue;
        const ex = a.x - b.x, ey = a.y - b.y, e2 = ex * ex + ey * ey + 1;
        a.vx += (ex / e2) * 900 * devicePixelRatio; a.vy += (ey / e2) * 900 * devicePixelRatio;
      }
    }
    for (const a of nodes) if (!a.fixed) { a.x += a.vx * 0.5; a.y += a.vy * 0.5; a.vx *= 0.6; a.vy *= 0.6; }
  }
  ctx.clearRect(0, 0, W, H);
  ctx.font = `${12 * devicePixelRatio}px system-ui`;
  for (const n of nodes.slice(1)) {
    ctx.strokeStyle = '#34405e'; ctx.lineWidth = (1 + 3 * (n.count / max)) * devicePixelRatio;
    ctx.beginPath(); ctx.moveTo(W / 2, H / 2); ctx.lineTo(n.x, n.y); ctx.stroke();
  }
  for (const n of nodes) {
    ctx.fillStyle = n.fixed ? '#7aa2f7' : '#9ece6a';
    ctx.beginPath(); ctx.arc(n.x, n.y, n.r * devicePixelRatio, 0, 7); ctx.fill();
    ctx.fillStyle = '#d8dee9';
    ctx.fillText(`${n.name}${n.count ? ' (' + n.count + ')' : ''}`, n.x + (n.r + 4) * devicePixelRatio, n.y + 4 * devicePixelRatio);
  }
  canvas.onclick = (ev) => {
    const rect = canvas.getBoundingClientRect();
    const x = (ev.clientX - rect.left) * devicePixelRatio, y = (ev.clientY - rect.top) * devicePixelRatio;
    const hit = nodes.find((n) => Math.hypot(n.x - x, n.y - y) < (n.r + 6) * devicePixelRatio);
    if (hit && !hit.fixed) onPick(hit.name);
  };
}

export default async function (root, [name]) {
  const prefix = h('input', { placeholder: 'filter by name prefix' });
  const list = h('div');
  const load = guard(async () => {
    const result = await call('GET', '/v1/entities?limit=100&prefix=' + encodeURIComponent(prefix.value));
    const entities = result.entities ?? [];
    list.replaceChildren(entities.length ? table([
      { label: 'entity', render: (e) => h('b', null, e.name) }, { label: 'type', render: (e) => (e.type ? badge(e.type) : '') },
      { label: 'mentions', key: 'mentions' }, { label: 'aliases', render: (e) => (e.aliases || []).join(', ') },
    ], entities, (e) => (location.hash = '#/graph/' + encodeURIComponent(e.name))) : h('p', null, 'No entities found.'));
  });
  prefix.addEventListener('input', load);
  const canvas = h('canvas');
  const detail = h('div');
  page(root, 'Entity graph', 'Entities come from facts and rule patterns; memories mentioning the same entity form the graph used by entity retrieval and graph hops.',
    h('p', null, h('a',{href:'#/documents'},'Explore documents, chunks, embeddings and revision lineage')),
    h('div', { class: 'grid2' },
      h('div', { class: 'card' }, h('div', { class: 'row' }, prefix), list),
      h('div', null, h('div', { class: 'card' }, canvas), detail)));
  await load();
  if (!name) return;
  const n = await call('GET', '/v1/entities/' + encodeURIComponent(name));
  draw(canvas, n.entity, n.related ?? [], (pick) => (location.hash = '#/graph/' + encodeURIComponent(pick)));
  detail.replaceChildren(h('div', { class: 'card' },
    h('h2', null, `${n.entity.name} · ${n.entity.mentions} mentions`),
    n.entity.type === 'document' ? h('a',{href:'#/documents/'+encodeURIComponent(n.entity.name)},'Open document entity and original file') : null,
    (n.memories ?? []).length ? table([
      { label: 'memory', render: (m) => m.text }, { label: 'state', render: status },
      { label: 'id', render: (m) => h('span', { class: 'mono' }, short(m.id)) },
    ], n.memories, (m) => (location.hash = '#/memory/' + m.id)) : h('p', null, 'No current readable memories mention this entity.')));
}
