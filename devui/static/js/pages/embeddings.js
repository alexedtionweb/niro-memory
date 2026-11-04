// Embeddings: declared models, coverage per model, backfill and the
// blue/green switch of the active model.
import { call, h, page, table, badge, guard, toast, pre } from '../lib.js';

export default async function (root) {
  const settings = await call('GET', '/v1/settings');
  const models = [...(settings.vector_models || [])];
  const capabilities = (await call('GET', '/_dev/api/info')).capabilities;
  const emb = capabilities.embedder;
  if (emb && !models.find((m) => m.id === emb)) models.unshift({ id: emb, dims: '(embedder)', active: !models.some((m) => m.active), implicit: true });
  const rows = await Promise.all(models.map(async (m) => {
    try {
      const c = await call('GET', '/v1/vectors/coverage?model=' + encodeURIComponent(m.id));
      return { ...m, ...c };
    } catch (e) { return { ...m, coverageError: e.message }; }
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
  page(root, 'Embeddings & semantic readiness', 'A vector index is storage, not a model. Semantic retrieval also needs query embeddings, document coverage, an active model, and a suitable query budget.',
    h('div', { class: 'card' },
      h('h2', null, emb ? (emb === 'hash@1' ? 'Test embedder only — not semantic' : 'Text embedder configured') : 'Automatic text embeddings are not configured'),
      h('p', null, badge(capabilities.vector ? 'Vector index available' : 'No vector index', capabilities.vector ? 'ok' : 'warn'),
        badge(emb || 'No embedder', emb && emb !== 'hash@1' ? 'ok' : 'warn')),
      h('p', { class: 'muted' }, emb === 'hash@1'
        ? 'Hash vectors exercise the vector path, but do not understand meaning. Configure a real embedding model for semantic recall.'
        : emb ? 'Check coverage below and the Search playground trace to confirm vectors actually participate in retrieval.'
          : 'Lexical and entity retrieval still work. Caller-supplied vectors are supported. To embed text automatically, configure an optional provider and restart the server.'),
      h('p', null, 'Existing memories need backfill. Local models may exceed the default 50 ms embedding budget; configure a tenant policy with a realistic budget and inspect skipped strategies in ',
        h('a', { href: '#/search' }, 'Search playground'), '.')),
    h('div', { class: 'card' },
      table([
        { label: 'model', render: (m) => [h('b', null, m.id), m.implicit ? badge('embedder') : null] },
        { label: 'dims', key: 'dims' }, { label: 'modalities', render: (m) => (m.modalities || ['text']).join(', ') },
        { label: 'coverage', render: (m) => {
          if (m.coverageError) return badge('Coverage failed: ' + m.coverageError, 'bad');
          const pct = m.total ? Math.round((100 * m.have) / m.total) : 0;
          const bar = h('div', { class: 'bar' }, h('span', { class: 'vec' }));
          bar.firstChild.style.width = pct + '%';
          return [bar, h('span', { class: 'muted' }, ` ${m.have}/${m.total} (${pct}%)`)];
        } },
        { label: 'state', render: (m) => (m.active ? badge('active', 'ok') : h('button', { disabled: !!m.coverageError, onclick: () => activate(m) }, 'make active')) },
      ], rows),
      h('div', { class: 'row' },
        emb ? h('button', { class: 'primary', onclick: guard(async () => { await call('POST', '/v1/vectors/backfill'); toast('Backfill queued — run the worker (Jobs page)'); }) }, 'Backfill missing with ' + emb)
          : h('p', { class: 'muted' }, 'No embedder configured: bring vectors via the API (PUT /v1/vectors, or vectors on facts and turns).'))),
    h('div', { class: 'card' }, h('h2', null, 'Optional providers'),
      h('p', null, 'Ollama native: use "ollama" with /api/embed. OpenAI, LM Studio, vLLM, TEI, and LiteLLM: use "openai" with an OpenAI-compatible /embeddings endpoint. No provider SDK is required.'),
      h('p', { class: 'muted' }, 'Example server config section (not tenant settings). Install the model in your Ollama service separately; the server never downloads models.'),
      pre({ embedder: { use: 'ollama', ollama: { base_url: 'http://127.0.0.1:11434', model: 'nomic-embed-text', dims: 768,
        query_prefix: 'search_query: ', document_prefix: 'search_document: ', keep_alive: '10m' } } }),
      h('p', { class: 'muted' }, 'The repository guide docs/embeddings.md covers other providers, model revisions, query budgets, and authentication.')),
    h('div', { class: 'card' }, h('h2', null, 'Declare a model'),
      h('p', { class: 'muted' }, 'Add to vector_models in Tenants & settings, e.g. {"id": "clip-vit-b32@1", "dims": 512, "modalities": ["text","image"]}. Vectors of undeclared models or wrong dimensions are rejected.')));
}
