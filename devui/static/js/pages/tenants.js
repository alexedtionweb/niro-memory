// Tenants & settings: switch tenant, inspect capabilities, edit the
// tenant settings document (rules override, vector models, bindings,
// retrieval policies).
import { state, call, h, page, table, pre, guard, toast } from '../lib.js';
import { workspaceSession } from '../knowledge-ui.js';

export default async function (root) {
  const active = workspaceSession(root);
  const tenants = await call('GET', '/_dev/api/tenants');
  if (!active()) return;
  const settings = await call('GET', '/v1/settings');
  if (!active()) return;
  const editor = h('textarea', { rows: 18 });
  editor.value = JSON.stringify(settings, null, 2);
  const save = guard(async () => {
    if (!active()) return;
    const doc = JSON.parse(editor.value);
    await call('PUT', '/v1/settings', doc);
    if (active()) { toast('Settings saved'); window.dispatchEvent(new Event('niromem:config')); }
  });
  const example = {
    vector_models: [{ id: 'openai/text-embedding-3-small@1', dims: 1536, modalities: ['text'], active: true }],
    bindings: [{ agent: 'sales-bot', users: ['alice'], read_agents: ['support-bot'] }],
    policies: { support: { name: 'support', fusion: 'additive', lexical: 1, vector: 1, entity: 0.5, prior_share: 0.2,
      recency: 1, confidence: 1, importance: 1, recency_half_life: 604800000000000, mmr_lambda: 0.7, embed_budget: 50000000, overfetch: 4, graph_hops: 1 } },
    default_policy: 'default',
  };
  const newTenant = h('input', { placeholder: 'new tenant id' });
  page(root, 'Tenants & settings', 'Every request is scoped to one tenant — a hard isolation boundary enforced by every store query.',
    h('div', { class: 'grid2' },
      h('div', { class: 'card' },
        h('h2', null, 'Tenants'),
        table([
          { label: 'tenant', render: (t) => h('b', null, t.tenant) },
          { label: 'memories', key: 'memories' }, { label: 'episodes', key: 'episodes' },
          { label: 'entities', key: 'entities' }, { label: 'jobs', key: 'jobs' },
        ], tenants || [], (t) => { state.tenant = t.tenant; window.dispatchEvent(new Event('niromem:tenant')); location.hash = '#/memories'; }),
        h('div', { class: 'row' }, newTenant, h('button', { onclick: () => {
          if (!newTenant.value) return;
          state.tenant = newTenant.value; window.dispatchEvent(new Event('niromem:tenant')); location.reload();
        } }, 'Use tenant')),
        h('p', { class: 'muted' }, 'Tenants exist implicitly; writing any record creates one. With API keys, the tenant comes from the key.'),
        h('h2', null, 'Capabilities'),
        pre(state.info || {}),
      ),
      h('div', { class: 'card' },
        h('h2', null, `Settings of “${state.tenant}”`),
        editor,
        h('div', { class: 'row' },
          h('button', { class: 'primary', onclick: save }, 'Save'),
          h('button', { onclick: () => { editor.value = JSON.stringify({ ...example, ...JSON.parse(editor.value || '{}') }, null, 2); } }, 'Insert example')),
        h('p', { class: 'muted' }, 'Rules: edit them on the Rules page. Durations are nanoseconds. Settings are cached by each node for a few seconds.'),
      )));
}
