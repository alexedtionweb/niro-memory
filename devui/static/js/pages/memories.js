// Memory browser: list memories with the full filter language, scope and a
// bi-temporal point of view.
import { call, h, page, table, short, status, badge, field, choose, guard, parseJSON } from '../lib.js';

const q = { user_id: '', agent_id: '', run_id: '', filter: '', valid_at: '', known_at: '', all_versions: false, show_expired: false, order: '', limit: 50 };

export default async function (root) {
  const results = h('div');
  const run = guard(async () => {
    const p = new URLSearchParams();
    for (const k of ['user_id', 'agent_id', 'run_id', 'valid_at', 'known_at', 'order']) if (q[k]) p.set(k, q[k]);
    if (q.filter.trim()) p.set('filter', JSON.stringify(parseJSON(q.filter)));
    if (q.all_versions) p.set('all_versions', 'true');
    if (q.show_expired) p.set('show_expired', 'true');
    p.set('limit', q.limit || 50);
    const t0 = performance.now();
    const { memories } = await call('GET', '/v1/memories?' + p);
    results.replaceChildren(
      h('p', { class: 'muted' }, `${memories.length} memories · ${(performance.now() - t0).toFixed(1)} ms round trip`),
      table([
        { label: 'id', render: (m) => h('span', { class: 'mono' }, short(m.id)) },
        { label: 'text', render: (m) => [m.title ? h('b', null, m.title + ' — ') : null, m.text] },
        { label: 'kind', render: (m) => [badge(m.kind), badge('L' + m.layer)] },
        { label: 'slot', key: 'slot' },
        { label: 'owner', render: (m) => [m.user_id, m.agent_id && ' · ' + m.agent_id, m.run_id && ' · ' + m.run_id] },
        { label: 'state', render: (m) => [status(m), m.version > 1 ? badge('v' + m.version) : null] },
        { label: 'trust', render: (m) => badge(m.source.trust, m.source.trust === 'low' ? 'warn' : '') },
        { label: 'heat', key: 'heat' },
      ], memories, (m) => (location.hash = '#/memory/' + m.id)));
  });
  page(root, 'Memory browser', 'Filter with the JSON filter language (Mem0-compatible): {"tags":{"contains":"travel"}, "OR":[…]}.',
    h('div', { class: 'card' },
      h('div', { class: 'row' },
        field('user_id', q, 'user_id'), field('agent_id', q, 'agent_id'), field('run_id', q, 'run_id'),
        choose('order', q, 'order', ['', 'oldest', 'heat']), field('limit', q, 'limit', { type: 'number', min: 1, max: 1000 })),
      h('div', { class: 'row' },
        field('valid at (world time)', q, 'valid_at', { placeholder: '2026-01-01' }),
        field('known at (record time)', q, 'known_at', { placeholder: '2026-01-01' }),
        field('all versions', q, 'all_versions', { type: 'checkbox' }), field('show expired', q, 'show_expired', { type: 'checkbox' })),
      field('filter (JSON)', q, 'filter', { textarea: true, rows: 3, placeholder: '{"kind": "semantic", "metadata.city": {"in": ["Lisbon", "Porto"]}}' }),
      h('div', { class: 'row' }, h('button', { class: 'primary', onclick: run }, 'List'))),
    results);
  run();
}
