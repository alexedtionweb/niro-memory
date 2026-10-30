// Embeddings: declared models, coverage per model, backfill and the
// blue/green switch of the active model.
import { state, call, h, page, table, badge, guard, toast } from '../lib.js';

export default async function (root) {
  const settings = await call('GET', '/v1/settings');
  const models = [...(settings.vector_models || [])];
  const emb = state.info?.capabilities?.embedder;
  if (emb && !models.find((m) => m.id === emb)) models.unshift({ id: emb, dims: '(embedder)', active: !models.some((m) => m.active), implicit: true });
  const rows = await Promise.all(models.map(async (m) => {
    try {
      const c = await call('GET', '/v1/vectors/coverage?model=' + encodeURIComponent(m.id));
      return { ...m, ...c };
    } catch (e) { return { ...m, have: '?', total: '?' }; }
  }));
  const activate = guard(async (m) => {
    if (m.total && m.have < m.total && !confirm(`Only ${m.have}/${m.total} memories have ${m.id} vectors. Switch anyway?`)) return;
    const s = await call('GET', '/v1/settings');
    s.vector_models = (s.vector_models || []).map((x) => ({ ...x, active: x.id === m.id }));
    if (!s.vector_models.find((x) => x.id === m.id)) s.vector_models.forEach((x) => (x.active = false));
    await call('PUT', '/v1/settings', s);
    toast('Active model: ' + m.id);
    location.reload();
  });
  page(root, 'Embeddings', 'Vectors are optional and bring-your-own. The store keeps them; external indexes are rebuildable projections. Switch models only at full coverage (blue/green).',
    h('div', { class: 'card' },
      table([
        { label: 'model', render: (m) => [h('b', null, m.id), m.implicit ? badge('embedder') : null] },
        { label: 'dims', key: 'dims' }, { label: 'modalities', render: (m) => (m.modalities || ['text']).join(', ') },
        { label: 'coverage', render: (m) => {
          const pct = m.total ? Math.round((100 * m.have) / m.total) : 0;
          const bar = h('div', { class: 'bar' }, h('span', { class: 'vec' }));
          bar.firstChild.style.width = pct + '%';
          return [bar, h('span', { class: 'muted' }, ` ${m.have}/${m.total} (${pct}%)`)];
        } },
        { label: 'state', render: (m) => (m.active ? badge('active', 'ok') : h('button', { onclick: () => activate(m) }, 'make active')) },
      ], rows),
      h('div', { class: 'row' },
        emb ? h('button', { class: 'primary', onclick: guard(async () => { await call('POST', '/v1/vectors/backfill'); toast('Backfill queued — run the worker (Jobs page)'); }) }, 'Backfill missing with ' + emb)
          : h('p', { class: 'muted' }, 'No embedder configured: bring vectors via the API (PUT /v1/vectors, or vectors on facts and turns).'))),
    h('div', { class: 'card' }, h('h2', null, 'Declare a model'),
      h('p', { class: 'muted' }, 'Add to vector_models in Tenants & settings, e.g. {"id": "clip-vit-b32@1", "dims": 512, "modalities": ["text","image"]}. Vectors of undeclared models or wrong dimensions are rejected.')));
}
