import { call, h, page, badge, pre, toast } from '../lib.js';
import { workspaceSession } from '../knowledge-ui.js';

export default async function(root) {
  const active = workspaceSession(root);
  const modelsView = h('div'), jobsView = h('div'), summary = h('div');
  const message = h('p', { role: 'status' });
  let busy = false, polling = false, timer;
  const refreshButton = h('button', { onclick: refresh }, 'Refresh coverage');
  const backfill = h('button', { class: 'primary', 'data-action': 'backfill', onclick: async () => {
    if (!active() || busy) return;
    busy = true; backfill.disabled = true;
    try {
      await call('POST', '/v1/vectors/backfill');
      if (!active()) return;
      polling = true;
      message.textContent = 'Backfill queued for existing memories. Watching real coverage; the server worker must be running.';
    } catch (e) {
      if (active()) message.textContent = 'Backfill failed: ' + e.message;
    } finally { busy = false; if (active()) await refresh(); }
  } }, 'Embed existing memories');

  async function activate(model) {
    if (!active() || busy) return;
    if (model.have < model.total && !confirm(`Only ${model.have}/${model.total} memories have these vectors. Switch anyway?`)) return;
    busy = true;
    try {
      const settings = await call('GET', '/v1/settings');
      if (!active()) return;
      settings.vector_models = (settings.vector_models || []).map(m => ({ ...m, active: m.id === model.id }));
      await call('PUT', '/v1/settings', settings);
      if (active()) { toast('Active model: ' + model.id); window.dispatchEvent(new Event('niromem:config')); }
    } catch (e) {
      if (active()) message.textContent = 'Model switch failed: ' + e.message;
    } finally { busy = false; if (active()) await refresh(); }
  }

  async function refresh() {
    if (!active() || busy) return;
    clearTimeout(timer);
    busy = true; refreshButton.disabled = true; backfill.disabled = true;
    try {
      const info = await call('GET', '/_dev/api/info');
      if (!active()) return;
      const embedder = info.capabilities?.embedder;
      if (!info.vectors || !Array.isArray(info.vectors.models)) throw new Error('Effective vector configuration is unavailable. Restart the server with the updated build.');
      const rows = await Promise.all(info.vectors.models.map(async model => {
        try {
          return { ...model, ...await call('GET', '/v1/vectors/coverage?model=' + encodeURIComponent(model.id)) };
        } catch (e) { return { ...model, error: e.message }; }
      }));
      if (!active()) return;
      summary.replaceChildren(
        h('h2', null, embedder && embedder !== 'hash@1' ? 'Semantic embedding model configured' : 'Semantic embedding model unavailable'),
        h('p', null, badge(embedder || 'No embedder', embedder && embedder !== 'hash@1' ? 'ok' : 'warn'),
          badge(info.capabilities.vector ? 'Vector index available' : 'No vector index', info.capabilities.vector ? 'ok' : 'warn')),
        h('p', null, 'Query model: ', h('strong', null, info.vectors.active || 'None')),
        h('p', { class: 'muted' }, 'Coverage is counted from stored vectors, not inferred from configuration. Backfill processes existing items; new writes are queued automatically.'));
      modelsView.replaceChildren(...rows.map(model => h('article', { class: 'embedding-model card' },
        h('h2', null, model.id, model.id === info.vectors.active ? badge('Active query model', 'ok') : null),
        h('p', { class: 'muted' }, `${model.dims} dimensions · ${(model.modalities || ['text']).join(', ')}`),
        model.error ? h('p', { class: 'error-message', role: 'alert' }, 'Coverage failed: ' + model.error)
          : h('div', null, h('progress', { max: Math.max(1, model.total), value: model.have, 'aria-label': 'Embedding coverage for ' + model.id }),
            h('p', null, h('strong', null, `${model.have}/${model.total}`), ' current memories embedded',
              model.total === 0 ? ' — no current memories in this tenant.' : model.have === model.total ? ' — ready.' : ` — ${model.total - model.have} missing.`)),
        model.id !== info.vectors.active ? h('button', { disabled: !!model.error, onclick: () => activate(model) }, 'Use for queries') : null)));
      backfill.disabled = !embedder || !info.capabilities.vector;
      if (!rows.length) modelsView.replaceChildren(h('p', null, 'No vector models declared. Configure an embedder in the server configuration.'));
      const current = rows.find(m => m.id === embedder);
      const covered = current && !current.error && current.have === current.total;
      try {
        const jobs = ((await call('GET', '/_dev/api/jobs')) || []).filter(j => j.kind === 'embed');
        if (!active()) return;
        jobsView.replaceChildren(h('h2', null, 'Embedding jobs'),
          ...(jobs.length ? jobs.map(j => h('p', { class: j.state === 'dead' ? 'error-message' : '' },
            badge(j.state, j.state === 'dead' ? 'bad' : ''), ` ${j.key} · attempts ${j.attempts}`,
            j.last_error ? h('span', null, ' · ' + j.last_error) : null)) : [h('p', { class: 'muted' }, 'No outstanding embedding jobs.')]),
          h('a', { href: '#/operations?view=jobs' }, 'Inspect jobs or retry a failed job'));
        if (jobs.some(j => ['pending', 'leased'].includes(j.state))) polling = true;
        else {
          if (polling && jobs.some(j => j.state === 'dead')) message.textContent = 'Backfill stopped. Inspect the failed embedding job below.';
          else if (polling && covered) message.textContent = 'Backfill complete. All current memories have vectors.';
          polling = false;
        }
      } catch (e) {
        if (active()) { polling = false; jobsView.replaceChildren(h('p', { class: 'error-message', role: 'alert' }, 'Job status unavailable: ' + e.message)); }
      }
    } catch (e) {
      if (active()) { polling = false; message.textContent = 'Embedding status failed: ' + e.message; }
    } finally {
      busy = false;
      if (active()) {
        refreshButton.disabled = false;
        if (polling) timer = setTimeout(refresh, 2000);
      }
    }
  }

  page(root, 'Embeddings', 'Make existing memories searchable by meaning, and inspect the actual coverage.',
    h('section', { class: 'card' }, summary, h('div', { class: 'row' }, backfill, refreshButton), message),
    modelsView, h('section', { class: 'card' }, jobsView),
    h('details', { class: 'card' }, h('summary', null, 'Provider configuration & semantic search'),
      h('p', null, 'Ollama uses /api/embed. OpenAI-compatible endpoints also support LM Studio, vLLM, TEI and LiteLLM. Configure providers on the server; model downloads are never automatic.'),
      h('p', null, 'Local CPU query embedding can exceed the default 50 ms retrieval budget. Set a suitable tenant policy and inspect the actual strategy trace in ', h('a', { href: '#/workspace?view=search' }, 'Search'), '.'),
      pre({ embedder: { use: 'ollama', ollama: { base_url: 'http://127.0.0.1:11434', model: 'nomic-embed-text', dims: 768,
        id: 'ollama/nomic-embed-text@1', query_prefix: 'search_query: ', document_prefix: 'search_document: ', keep_alive: '10m' } } })));
  await refresh();
}
