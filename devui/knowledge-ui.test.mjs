import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

// Exercise the actual page modules and event handlers without a frontend
// dependency. Browser layout/CSP are checked separately against the server.
class Element {
  constructor(tag = '') {
    this.tag = tag; this.children = []; this.attributes = {}; this.listeners = {};
    this.value = ''; this.checked = false; this.disabled = false; this.style = {};
  }
  get isConnected() { return this.mounted || !!this.parent?.isConnected; }
  get textContent() { return this.text || this.children.map(c => c.textContent).join(''); }
  set textContent(value) { this.children = []; this.text = String(value); }
  append(...children) {
    this.text = '';
    for (let child of children) {
      if (!(child instanceof Element)) { const text = new Element(); text.textContent = child; child = text; }
      child.parent = this; this.children.push(child);
      if (this.tag === 'select' && (child.attributes.selected !== undefined || this.children.length === 1)) this.value = child.attributes.value || '';
    }
  }
  replaceChildren(...children) { this.children.forEach(c => { c.parent = null; }); this.children = []; this.text = ''; this.append(...children); }
  setAttribute(name, value) {
    this.attributes[name] = String(value);
    if (name === 'disabled') this.disabled = true;
    if (name === 'value') this.value = String(value);
  }
  addEventListener(name, callback) { (this.listeners[name] ||= []).push(callback); }
  async fire(name) { if (name === 'click' && this.disabled) return; await Promise.all((this.listeners[name] || []).map(fn => fn({ target: this }))); }
  querySelectorAll(selector) {
    const [tag, attr, value] = selector.match(/^([a-z0-9-]+)?(?:\[([^=]+)="([^"]+)"\])?$/).slice(1);
    return this.children.flatMap(child => [
      ...((!tag || child.tag === tag) && (!attr || child.attributes[attr] === value) ? [child] : []),
      ...child.querySelectorAll(selector),
    ]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  focus() { globalThis.document.activeElement = this; }
  remove() { if (this.parent) this.parent.children = this.parent.children.filter(c => c !== this); this.parent = null; }
}
globalThis.Node = Element;
globalThis.window = { dispatchEvent() {} };
const toast = new Element('div');
globalThis.document = {
  createElement: tag => new Element(tag), createElementNS: (_, tag) => new Element(tag),
  createTextNode: text => { const n = new Element(); n.textContent = text; return n; },
  getElementById: () => toast,
};
const storage = () => { const values = new Map(); return { getItem: k => values.get(k), setItem: (k, v) => values.set(k, v) }; };
globalThis.localStorage = storage(); globalThis.sessionStorage = storage();

async function moduleURL(path) {
  let source = await readFile(new URL(path, import.meta.url), 'utf8');
  for (const match of [...source.matchAll(/from ['"](\.[^'"]+)['"]/g)]) {
    const url = new URL(match[1], new URL(path, import.meta.url));
    source = source.replace(match[1], await moduleURL(url));
  }
  return 'data:text/javascript;base64,' + Buffer.from(source).toString('base64');
}
const lib = await import(await moduleURL('./static/js/lib.js'));
const ui = await import(await moduleURL('./static/js/knowledge-ui.js'));
const playground = (await import(await moduleURL('./static/js/pages/intelligence.js'))).default;
const inspector = (await import(await moduleURL('./static/js/pages/knowledge-state.js'))).default;
const transitions = (await import(await moduleURL('./static/js/pages/transitions.js'))).default;
const explorer = (await import(await moduleURL('./static/js/pages/explorer.js'))).default;
const memoryDetail = (await import(await moduleURL('./static/js/pages/memory.js'))).default;
const schemaPage = (await import(await moduleURL('./static/js/pages/schema.js'))).default;
const embeddingsPage = (await import(await moduleURL('./static/js/pages/embeddings.js'))).default;
const timelinePage = (await import(await moduleURL('./static/js/pages/timeline.js'))).default;
const tenantsPage = (await import(await moduleURL('./static/js/pages/tenants.js'))).default;
const jobsPage = (await import(await moduleURL('./static/js/pages/jobs.js'))).default;
const searchPage = (await import(await moduleURL('./static/js/pages/search.js'))).default;
const graphPage = (await import(await moduleURL('./static/js/pages/graph.js'))).default;
const { documentImport } = await import(await moduleURL('./static/js/document-import.js'));
const { filePreview } = await import(await moduleURL('./static/js/file-preview.js'));
const config = () => ({ server: { mode: 'disabled', max_candidates: 16, max_graph_hops: 2 },
  effective: { mode: 'enforced', provider: 'deterministic' }, policy: null, runtime: true,
  profiles: { deterministic: { provider: 'deterministic' }, model: { provider: 'ollama', model: 'test-model' } } });
const receipt = status => ({ status, mode: status === 'shadow' ? 'shadow' : 'enforced', validation: 'passed',
  plan: { operation_key: 'op', provider: 'deterministic', operations: [{ type: 'ADD', reason_code: 'explicit_add', memory: { text: 'test assertion' } }] },
  proposed_plan: { operations: [{ type: 'ADD' }] }, changes: status === 'committed' ? [{ action: 'add', memory: { id: 'm', text: 'test assertion' } }] : [] });
const memory = { id: 'm', text: 'test assertion', user_id: 'u', agent_id: 'a', version: 1, kind: 'semantic', source: {} };

function setup(handler, statusHandler = () => ({ capabilities: {}, knowledge: config() })) {
  lib.state.tenant = 'a'; lib.state.key = ''; lib.state.info = { capabilities: {} };
  const calls = [], root = new Element('div'); root.mounted = true;
  globalThis.fetch = async (path, options) => {
    const request = { path, method: options.method, body: options.body ? JSON.parse(options.body) : undefined, headers: options.headers };
    calls.push(request);
    const response = await (path === '/_dev/api/info' || path.startsWith('/v1/vectors/coverage') ? statusHandler(request) : handler(request));
    const failed = !!response?.error && !response?.status;
    return { ok: !failed, status: failed ? 400 : 200, text: async () => JSON.stringify(response) };
  };
  return { root, calls };
}
const labelled = (root, label) => { const el = root.querySelectorAll(`[aria-label="${label === 'Confirm durable commit' ? 'Confirm this action' : label}"]`)[0]; assert(el, label); return el; };
const button = (root, text) => {
  const action = { 'Preview validated transition': 'preview', 'Commit / retry same operation': 'commit', 'New operation key': 'reset' }[text];
  const el = root.querySelectorAll('button').find(n => n.textContent === text || action && n.attributes['data-action'] === action);
  assert(el, text); return el;
};
async function edit(root, json = { scope: { user_id: 'u' }, text: 'test assertion', visibility: 'team', trust: 'high', intent: 'ADD' }) {
  const toggle = labelled(root, 'Use JSON instead of the form');
  if (!toggle.checked) { toggle.checked = true; await toggle.fire('change'); }
  const el = labelled(root, 'Knowledge input JSON'); el.value = JSON.stringify(json); await el.fire('input');
}
const preview = root => button(root, 'Preview validated transition').fire('click');
async function confirm(root) { const el = labelled(root, 'Confirm durable commit'); el.checked = true; await el.fire('change'); }
const commit = root => button(root, 'Commit / retry same operation').fire('click');

test('jobs cannot start a competing manual worker when inline processing is enabled', async () => {
  const { root, calls } = setup(() => []);
  lib.state.info = { inline_worker: true };
  await jobsPage(root);
  const automatic = button(root, 'Worker running automatically');
  assert(automatic.disabled);
  await automatic.fire('click');
  assert(!calls.some(c => c.method === 'POST'));
  root.mounted = false;
});

test('job retry updates the same job and exposes pause and cancel controls', async () => {
  let state = 'dead';
  toast.replaceChildren();
  const { root, calls } = setup(({ method, body }) => {
    if (method === 'POST') { state = body.action === 'pause' ? 'paused' : 'pending'; return { id: 'dead-job', state }; }
    return [{ id: 'dead-job', tenant: 'a', state, kind: 'knowledge.enrich', key: 'source', attempts: 1 }];
  });
  await jobsPage(root);
  await button(root, 'Retry').fire('click');
  assert(toast.textContent.includes('Retry: accepted'));
  assert(!root.querySelectorAll('button').some(b => b.textContent === 'Retry'));
  await button(root, 'Pause').fire('click');
  assert(button(root, 'Resume'));
  assert(button(root, 'Cancel'));
  assert.equal(calls.find(c => c.method === 'POST').path, '/_dev/api/jobs/control');
  assert.equal(calls.find(c => c.method === 'POST').body.id, 'dead-job');
  assert.equal(calls.filter(c => c.method === 'POST').length, 2);
  root.mounted = false;
});

test('tenant cleanup needs exact typed name, final confirmation and tenant-wide endpoint', async () => {
  const { root, calls } = setup(({ path }) => path === '/v1/maintenance/purge'
    ? { id: 'cleanup', state: 'purging', deleted: 0 }
    : { memories: [], more: false });
  await explorer(root, []);
  const remove = button(root, 'Delete all tenant data');
  assert(remove.disabled);
  const input = labelled(root, 'Type tenant name to delete everything');
  input.value = 'wrong'; await input.fire('input');
  assert(remove.disabled);
  input.value = 'a'; await input.fire('input');
  assert(!remove.disabled);
  window.confirm = () => false;
  await remove.fire('click');
  assert(!calls.some(c => c.method === 'POST'));
  window.confirm = () => true;
  await remove.fire('click');
  assert.equal(calls.find(c => c.method === 'POST').body.confirmation, 'a');
  assert(calls.find(c => c.method === 'POST').body.request_id);
  assert(remove.disabled);
  assert(root.textContent.includes('Writes are locked until completion'));
  root.mounted = false;
});

test('job pagination uses keyset cursors rather than growing offsets', async () => {
  const { root, calls } = setup(() => Array.from({ length: 101 }, (_, i) =>
    ({ id: `job-${String(i).padStart(3, '0')}`, tenant: 'a', state: 'pending', attempts: 0 })));
  await jobsPage(root);
  await button(root, 'Next jobs').fire('click');
  assert(calls.at(-1).path.includes('after_id=job-099'));
  assert(!calls.some(c => c.path.includes('offset=')));
  await button(root, 'Previous jobs').fire('click');
  assert(calls.at(-1).path.includes('after_id='));
  root.mounted = false;
});

test('file work appears once with expandable child stages and one preview', async () => {
  const file = { id: 'doc-one', name: 'Report.pdf' };
  const { root } = setup(() => [
    { id: 'parse', tenant: 'a', kind: 'document.import', state: 'leased', attempts: 1, file },
    { id: 'enrich', tenant: 'a', kind: 'knowledge.enrich', state: 'pending', attempts: 0, file },
    { id: 'shared', tenant: 'a', kind: 'embed', state: 'pending', attempts: 0 },
  ]);
  await jobsPage(root);
  assert.equal(root.querySelectorAll('h2').filter(el => el.textContent === 'Report.pdf').length, 1);
  assert(root.querySelectorAll('summary').some(el => el.textContent === 'Processing stages (2 on this page)'));
  assert.equal(root.querySelectorAll('button').filter(el => el.textContent === 'Preview file').length, 1);
  assert(root.textContent.includes('Other / shared jobs'));
  root.mounted = false;
});

test('file preview fetches with credentials, escapes text and reports unsupported formats', async () => {
  const { root } = setup(() => ({}));
  lib.state.key = 'preview-key';
  const requests = [];
  globalThis.fetch = async (path, options) => {
    requests.push({ path, options });
    return { ok: true, blob: async () => new Blob(['<script>plain evidence</script>'], { type: 'text/plain' }) };
  };
  root.append(filePreview({ id: 'doc /one', name: 'Evidence.md' }, () => root.mounted));
  await button(root, 'Preview file').fire('click');
  assert.equal(requests[0].path, '/v1/documents/doc%20%2Fone/preview');
  assert.equal(requests[0].options.headers.Authorization, 'Bearer preview-key');
  assert.equal(root.querySelector('pre').textContent, '<script>plain evidence</script>');
  await button(root, 'Close preview').fire('click');
  assert(!root.querySelector('pre'));
  globalThis.fetch = async () => ({ ok: false, status: 400, text: async () => JSON.stringify({ error: 'Download the original; no inline preview.' }) });
  await button(root, 'Preview file').fire('click');
  assert(root.textContent.includes('Download the original; no inline preview.'));
  root.mounted = false;
});

test('image preview keeps its URL through refresh and releases it on close', async () => {
  const { root } = setup(() => ({}));
  const created = [], revoked = [];
  const originalCreate = URL.createObjectURL, originalRevoke = URL.revokeObjectURL;
  URL.createObjectURL = () => { const url = `blob:fixture-${created.length}`; created.push(url); return url; };
  URL.revokeObjectURL = url => revoked.push(url);
  try {
    globalThis.fetch = async () => ({ ok: true, blob: async () => new Blob(['fixture'], { type: 'image/png' }) });
    const component = filePreview({ id: 'doc-image', name: 'Image.png' }, () => root.mounted);
    root.append(component);
    await button(root, 'Preview file').fire('click');
    assert.equal(root.querySelector('img').attributes.src, 'blob:fixture-0');
    root.replaceChildren(component);
    assert.equal(root.querySelector('img').attributes.src, 'blob:fixture-0');
    await button(root, 'Close preview').fire('click');
    assert.deepEqual(revoked, ['blob:fixture-0']);
    assert(!root.querySelector('img'));
  } finally {
    root.mounted = false;
    URL.createObjectURL = originalCreate; URL.revokeObjectURL = originalRevoke;
  }
});

test('destructive and job controls use the authenticated tenant with API keys', async () => {
  const { root, calls } = setup(({ path, method }) => path.startsWith('/_dev/api/jobs')
    ? method === 'POST' ? { state: 'paused' } : [{ id: 'key-job', tenant: 'key-tenant', state: 'pending', attempts: 0 }]
    : { memories: [], more: false });
  lib.state.info = { tenant: 'key-tenant', capabilities: {} };
  lib.state.key = 'test-key';
  await jobsPage(root);
  assert(!button(root, 'Pause').disabled);
  await button(root, 'Pause').fire('click');
  assert.equal(calls.find(c => c.method === 'POST').headers.Authorization, 'Bearer test-key');
  root.mounted = false;
  const { root: browser } = setup(() => ({ memories: [], more: false }));
  lib.state.info = { tenant: 'key-tenant', capabilities: {} };
  lib.state.key = 'test-key';
  await explorer(browser, []);
  const name = labelled(browser, 'Type tenant name to delete everything');
  name.value = 'a'; await name.fire('input');
  assert(button(browser, 'Delete all tenant data').disabled);
  name.value = 'key-tenant'; await name.fire('input');
  assert(!button(browser, 'Delete all tenant data').disabled);
  browser.mounted = false;
});

test('historical browsing includes expired records and reveals them with older versions', async () => {
  const { root, calls } = setup(() => ({ memories: [memory, { ...memory, id: 'expired', text: 'Expired old assertion',
    expires_at: '2000-01-01T00:00:00Z' }], inferred_links: [], more: false }));
  await explorer(root, []);
  assert(calls[0].path.includes('show_expired=true'));
  assert(!root.querySelectorAll('button').some(b => b.textContent.includes('Expired old assertion')));
  const history = labelled(root, 'Include older versions');
  history.checked = true; await history.fire('change');
  assert(root.querySelectorAll('button').some(b => b.textContent.includes('Expired old assertion')));
});

test('timeline rejects stale scope responses and requests full temporal history', async () => {
  let finish;
  const { root, calls } = setup(() => new Promise(resolve => { finish = resolve; }));
  const pending = timelinePage(root);
  lib.state.tenant = 'other';
  finish({ memories: [{ ...memory, text: 'OLD TENANT DATA', recorded_at: '2020-01-01T00:00:00Z' }], relationships: [], more: false });
  await pending;
  assert(calls[0].path.includes('show_expired=true'));
  assert(!root.textContent.includes('OLD TENANT DATA'));
});

test('memory mutation controls cannot act in a different tenant or after navigation', async () => {
  const { root, calls } = setup(({ path }) => path.endsWith('/versions') ? { versions: [memory] } :
    path.endsWith('/history') ? { events: [] } : memory);
  await memoryDetail(root, ['m']);
  const before = calls.length;
  lib.state.tenant = 'other';
  for (const label of ['Save correction', 'Invalidate (ended now)', 'Delete']) await button(root, label).fire('click');
  assert.equal(calls.length, before);
  lib.state.tenant = 'a'; root.mounted = false;
  await button(root, 'Save correction').fire('click');
  assert.equal(calls.length, before);
});

test('tenant settings cannot save stale data to a newly selected tenant', async () => {
  const { root, calls } = setup(({ path }) => path === '/_dev/api/tenants' ? [] : { bindings: [{ agent: 'private' }] });
  await tenantsPage(root);
  const before = calls.length;
  lib.state.tenant = 'other';
  await button(root, 'Save').fire('click');
  assert.equal(calls.length, before);
});

test('dedicated schema view exposes the effective JSON schema and profile without opening disclosures', async () => {
  const value = config();
  value.effective.extraction = { entity_types: ['person', 'project'], fact_types: ['fact'] };
  value.effective.extraction_schema = { type: 'object', properties: { facts: { type: 'array' } } };
  const { root } = setup(({ path }) => {
    if (path.includes('/config')) return value;
    throw new Error(path);
  });
  await schemaPage(root);
  assert(root.textContent.includes('Effective JSON Schema'));
  assert(root.textContent.includes('"properties"'));
  assert(root.textContent.includes('person'));
  assert(labelled(root, 'Extraction profile JSON').disabled);
  assert(!root.querySelectorAll('button').some(b => b.attributes['data-action'] === 'save-now'));
});

const vectorInfo = () => ({ capabilities: { vector: true, embedder: 'local@1' },
  vectors: { active: 'local@1', models: [{ id: 'local@1', dims: 768, active: true }] } });

test('embedding status accepts empty job queues serialized as null by older servers', async () => {
  const { root } = setup(() => null, ({ path }) => path.includes('/coverage') ? { have: 2, total: 2 } : vectorInfo());
  await embeddingsPage(root);
  assert(root.textContent.includes('No outstanding embedding jobs.'));
  assert(!root.textContent.includes('Job status unavailable'));
  root.mounted = false;
});

test('embedding backfill uses actual coverage, preserves tenant headers and reports completion', async () => {
  let have = 1;
  const { root, calls } = setup(({ path }) => {
    if (path === '/v1/vectors/backfill') { have = 3; return { status: 'queued' }; }
    if (path === '/_dev/api/jobs') return [];
    throw new Error(path);
  }, ({ path }) => path.includes('/coverage') ? { have, total: 3 } : vectorInfo());
  await embeddingsPage(root);
  assert(root.textContent.includes('1/3'));
  assert(root.textContent.includes('768 dimensions'));
  await button(root, 'Embed existing memories').fire('click');
  assert(root.textContent.includes('3/3'));
  assert(root.textContent.includes('Backfill complete'));
  assert.equal(calls.filter(c => c.path === '/v1/vectors/backfill').length, 1);
  assert(calls.every(c => c.headers['X-Tenant'] === 'a'));
  root.mounted = false;
});

test('embedding coverage errors and failed jobs remain explicit, never ready-shaped', async () => {
  const { root } = setup(() => [{ kind: 'embed', key: 'local@1', state: 'dead', attempts: 3, last_error: 'Ollama refused connection' }],
    ({ path }) => path.includes('/coverage') ? { error: 'coverage unavailable' } : vectorInfo());
  await embeddingsPage(root);
  assert(root.textContent.includes('Coverage failed: coverage unavailable'));
  assert(root.textContent.includes('Ollama refused connection'));
  assert(!root.textContent.includes('— ready.'));
  root.mounted = false;
});

test('embedding controls cannot enqueue work after tenant switch', async () => {
  const { root, calls } = setup(() => [], ({ path }) => path.includes('/coverage') ? { have: 1, total: 2 } : vectorInfo());
  await embeddingsPage(root);
  lib.state.tenant = 'b';
  await button(root, 'Embed existing memories').fire('click');
  assert(!calls.some(c => c.path === '/v1/vectors/backfill'));
  root.mounted = false;
});

test('activating the implicit embedder clears explicit active overrides without dropping settings', async () => {
  let selected = 'external@1', saved;
  const { root } = setup(({ path, method, body }) => {
    if (path === '/_dev/api/jobs') return [];
    if (path === '/v1/settings' && method === 'GET') return {
      bindings: [{ agent: 'private' }], vector_models: [{ id: 'external@1', dims: 3, active: true }],
    };
    if (path === '/v1/settings' && method === 'PUT') { saved = body; selected = 'local@1'; return {}; }
    throw new Error(path);
  }, ({ path }) => path.includes('/coverage') ? { have: 2, total: 2 } : {
    capabilities: { vector: true, embedder: 'local@1' },
    vectors: { active: selected, models: [{ id: 'local@1', dims: 768 }, { id: 'external@1', dims: 3 }] },
  });
  await embeddingsPage(root);
  await button(root, 'Use for queries').fire('click');
  assert.deepEqual(saved.bindings, [{ agent: 'private' }]);
  assert.equal(saved.vector_models[0].active, false);
  assert(root.textContent.includes('Active query model'));
  root.mounted = false;
});

test('backfill enqueue failure stays explicit and can be retried', async () => {
  const { root } = setup(({ path }) => path === '/v1/vectors/backfill' ? { error: 'queue unavailable' } : [],
    ({ path }) => path.includes('/coverage') ? { have: 0, total: 1 } : vectorInfo());
  await embeddingsPage(root);
  await button(root, 'Embed existing memories').fire('click');
  assert(root.textContent.includes('Backfill failed: queue unavailable'));
  assert(!root.textContent.includes('Backfill complete'));
  assert(!button(root, 'Embed existing memories').disabled);
  root.mounted = false;
});

test('full coverage does not claim queued backfill work has completed', async () => {
  let queued = false, finished = false;
  const { root } = setup(({ path }) => {
    if (path === '/v1/vectors/backfill') { queued = true; return { status: 'queued' }; }
    if (path === '/_dev/api/jobs') return queued && !finished ? [{ kind: 'embed', key: 'local@1', state: 'pending', attempts: 0 }] : [];
    throw new Error(path);
  }, ({ path }) => path.includes('/coverage') ? { have: 2, total: 2 } : vectorInfo());
  await embeddingsPage(root);
  await button(root, 'Embed existing memories').fire('click');
  assert(root.textContent.includes('Backfill queued'));
  assert(!root.textContent.includes('Backfill complete'));
  finished = true;
  await button(root, 'Refresh coverage').fire('click');
  assert(root.textContent.includes('Backfill complete'));
  root.mounted = false;
});

test('shared DOM handlers report malformed JSON instead of unhandled promise rejections', async () => {
  const action = lib.h('button', { onclick: () => JSON.parse('{bad') }, 'Broken JSON');
  await action.fire('click');
  assert(toast.textContent.includes('JSON'));
});

test('playground performs real preview, explicit commit and stable replay; edits invalidate previews', async () => {
  let writes = 0;
  const { root, calls } = setup(({ path }) => {
    if (path.includes('/config')) return config();
    if (path.includes('/state')) return writes ? { current: [memory] } : {};
    if (path.endsWith('/evaluate')) return receipt('proposed');
    if (path.endsWith('/commit')) return { ...receipt('committed'), replayed: writes++ > 0 };
    throw new Error(path);
  });
  await playground(root, []); await edit(root);
  assert(button(root, 'Commit / retry same operation').disabled);
  await preview(root);
  assert.equal(writes, 0); assert(root.textContent.includes('Preview only — nothing saved'));
  await confirm(root); await commit(root); await commit(root);
  const posts = calls.filter(r => r.path.endsWith('/commit'));
  assert.equal(posts.length, 2); assert.deepEqual(posts[0].body, posts[1].body);
  assert(labelled(root, 'Knowledge input JSON').disabled);
  assert(root.textContent.includes('After this attempt'));
  assert(root.textContent.includes('idempotent replay'));
  assert(root.querySelectorAll('a').some(a => a.attributes.href.startsWith('#/transitions/')));
  const oldKey = labelled(root, 'Stable operation key').value;
  await button(root, 'New operation key').fire('click');
  assert.notEqual(labelled(root, 'Stable operation key').value, oldKey);
  await edit(root); await preview(root); await edit(root, { text: 'changed' });
  assert(button(root, 'Commit / retry same operation').disabled);
});

test('lost commit response locks the exact payload and retries safely', async () => {
  let attempts = 0;
  const { root, calls } = setup(({ path }) => {
    if (path.includes('/config')) return config();
    if (path.includes('/state')) return {};
    if (path.endsWith('/evaluate')) return receipt('proposed');
    if (path.endsWith('/commit')) { if (++attempts === 1) throw new Error('connection lost'); return { ...receipt('committed'), replayed: true }; }
    if (path === '/v1/settings') return {};
    throw new Error(path);
  });
  await playground(root, []); await edit(root); await preview(root); await confirm(root); await commit(root);
  assert(root.textContent.includes('Commit not confirmed: connection lost'));
  assert(labelled(root, 'Knowledge input JSON').disabled);
  await button(root, 'Save tenant intelligence configuration').fire('click');
  assert(root.textContent.includes('Start a new operation before changing configuration'));
  await commit(root);
  assert.deepEqual(...calls.filter(c => c.path.endsWith('/commit')).map(c => c.body));
});

test('successful commit remains visible when follow-up state fetch fails', async () => {
  let committed = false;
  const { root } = setup(({ path }) => {
    if (path.includes('/config')) return config();
    if (path.includes('/state')) return committed ? { error: 'state unavailable' } : {};
    if (path.endsWith('/evaluate')) return receipt('proposed');
    if (path.endsWith('/commit')) { committed = true; return receipt('committed'); }
    throw new Error(path);
  });
  await playground(root, []); await edit(root); await preview(root); await confirm(root); await commit(root);
  assert(root.textContent.includes('What happened'));
  assert(root.textContent.includes('Commit succeeded, but state refresh failed'));
});

test('invalid input and backend errors are explicit, not successful previews', async () => {
  const { root, calls } = setup(({ path }) => path.includes('/config') ? config() : path.includes('/state') ? {} : { error: 'knowledge: invalid_grants' });
  await playground(root, []);
  await edit(root);
  labelled(root, 'Knowledge input JSON').value = '{bad';
  await preview(root);
  assert.equal(calls.filter(c => c.method === 'POST').length, 0);
  await edit(root); await preview(root);
  assert(root.textContent.includes('invalid_grants'));
  assert(button(root, 'Commit / retry same operation').disabled);
});

test('disabled and shadow modes are represented honestly', async () => {
  for (const mode of ['disabled', 'shadow']) {
    const { root, calls } = setup(({ path }) => {
      if (path.includes('/config')) { const c = config(); c.effective.mode = mode; return c; }
      if (path.includes('/state')) return {};
      return receipt(path.endsWith('/evaluate') ? 'proposed' : 'shadow');
    });
    await playground(root, []); await edit(root);
    if (mode === 'disabled') {
      assert(button(root, 'Preview validated transition').disabled);
      assert(root.textContent.includes('State and historical traces remain readable'));
      assert.equal(calls.filter(c => c.method === 'POST').length, 0);
    } else {
      await preview(root); await confirm(root); await commit(root);
      assert(root.textContent.includes('without changing memory state'));
      assert(root.textContent.includes('Test result recorded — memories unchanged'));
    }
  }
});

test('runtime configuration switches modes/providers and preserves unrelated tenant settings', async () => {
  const settings = { default_policy: 'deep', bindings: [{ agent: 'existing' }] };
  let saved;
  const { root, calls } = setup(({ path, method, body }) => {
    if (path.includes('/config')) return config();
    assert.equal(path, '/v1/settings');
    if (method === 'PUT') saved = body;
    return method === 'GET' ? settings : {};
  });
  await playground(root, []);
  labelled(root, 'Tenant intelligence mode').value = 'shadow';
  labelled(root, 'Planner profile').value = 'model';
  labelled(root, 'Maximum candidates').value = '8';
  labelled(root, 'Maximum graph hops').value = '1';
  await button(root, 'Save tenant intelligence configuration').fire('click');
  assert.deepEqual(saved, { ...settings, knowledge: { mode: 'shadow', planner: 'model', max_candidates: 8, max_graph_hops: 1 } });
  assert(calls.every(c => c.headers['X-Tenant'] === 'a'));
  assert(root.textContent.includes('Saved for the authenticated tenant'));
});

test('configuration failure is reported; server-only permissions cannot be elevated in UI', async () => {
  const { root } = setup(({ path }) => {
    if (path.includes('/config')) { const c = config(); c.runtime = false; c.profiles = {}; c.server.mode = 'shadow'; return c; }
    if (path === '/v1/settings') return { error: 'permission denied' };
    throw new Error(path);
  });

  await playground(root, []);
  assert(!labelled(root, 'Tenant intelligence mode').children.some(c => c.value === 'enforced'));
  await button(root, 'Save tenant intelligence configuration').fire('click');
  assert(root.textContent.includes('Configuration error: permission denied'));
});

test('extraction profile editor preserves other policy fields, displays real schema and supports inheriting defaults', async () => {
    const profile = { entity_types: ['person', 'project'], fact_types: ['fact'], entity_attributes: { role: { type: 'string' } } };
    const settings = { default_policy: 'deep', knowledge: { mode: 'enforced', extraction: profile, future_field: 'keep' } };
    const c = config(); c.policy = settings.knowledge; c.effective.extraction = profile;
    c.effective.extraction_schema = { type: 'object', title: 'actual-provider-schema' };
    let saved;
    const { root } = setup(({ path, method, body }) => {
      if (path.includes('/config')) return c;
      if (path === '/v1/settings') { if (method === 'PUT') saved = body; return method === 'GET' ? settings : {}; }
      throw new Error(path);
    });
    await playground(root, []);
    assert(root.textContent.includes('actual-provider-schema'));
    const editor = labelled(root, 'Extraction profile JSON');
    assert(!editor.disabled); assert.deepEqual(JSON.parse(editor.value), profile);
    labelled(root, 'Tenant intelligence mode').value = 'shadow';
    await button(root, 'Save tenant intelligence configuration').fire('click');
    assert.deepEqual(saved.knowledge.extraction, profile);
    assert.equal(saved.knowledge.future_field, 'keep');
    const changed = { ...profile, entity_attributes: { owner: { type: 'string', enum: ['team'] } } };
    editor.value = JSON.stringify(changed);
    await button(root, 'Save tenant intelligence configuration').fire('click');
    assert.deepEqual(saved.knowledge.extraction, changed);
    const inherit = labelled(root, 'Use server extraction defaults');
    inherit.checked = true; await inherit.fire('change');
    assert(editor.disabled);
    await button(root, 'Save tenant intelligence configuration').fire('click');
    assert(!('extraction' in saved.knowledge)); assert.equal(saved.knowledge.future_field, 'keep');
  });

  test('invalid extraction profile JSON never writes settings and server validation errors are explicit', async () => {
    let puts = 0;
    const { root } = setup(({ path, method }) => {
      if (path.includes('/config')) return config();
      if (path === '/v1/settings') { if (method === 'PUT') { puts++; return { error: 'invalid extraction profile' }; } return {}; }
      throw new Error(path);
    });
    await playground(root, []);
    const inherit = labelled(root, 'Use server extraction defaults');
    inherit.checked = false; await inherit.fire('change');
    const editor = labelled(root, 'Extraction profile JSON');
    editor.value = '{';
    await button(root, 'Save tenant intelligence configuration').fire('click');
    assert.equal(puts, 0); assert(root.textContent.includes('Configuration error:'));
    editor.value = '{}';
    await button(root, 'Save tenant intelligence configuration').fire('click');
    assert.equal(puts, 1); assert(root.textContent.includes('invalid extraction profile'));
  });

  test('entity types, attributes and duplicate reinforcement are rendered only from actual extraction data', () => {
    const view = ui.renderEnrichment({ status: 'completed', extraction: { facts: [], entities: [
      { name: 'Alice', type: 'person', quote: 'Alice owns Atlas.', attributes: { role: 'owner' } },
    ], normalization_notes: ['Candidate context was excluded from source claims.'] }, changes: [{ action: 'evidence', memory: { id: 'existing' } }] });
    assert(view.textContent.includes('Alice')); assert(view.textContent.includes('person'));
    assert(view.textContent.includes('owner')); assert(view.textContent.includes('no duplicate created'));
    assert(view.textContent.includes('Candidate context was excluded from source claims.'));
    assert(view.querySelectorAll('a').some(a => a.attributes.href === '#/memory/existing'));
    const empty = ui.renderEnrichment({ status: 'completed', extraction: { facts: [], entities: [] } });
    assert(!empty.textContent.includes('Named entities')); assert(!empty.textContent.includes('no duplicate created'));
  });
test('state inspector sends both clocks and clearing Known at preserves Valid at', async () => {
  const { root, calls } = setup(() => ({ current: [memory], historical: [{ ...memory, id: 'old' }],
    conflicts: [memory], decisions: [memory], supporting_evidence: [memory],
    relations: [{ source_id: 'm', target_id: 'old', relation: 'corrects', origin: 'validated', operation_key: 'correction' }],
    unresolved_relationships: [{ source_id: 'm', target_id: 'old', relation: 'contradicts' }] }));
  await inspector(root, []);
  const dates = root.querySelectorAll('input[type="datetime-local"]');
  dates[0].value = '2026-01-02T03:04'; await dates[0].fire('input');
  dates[1].value = '2026-01-03T03:04'; await dates[1].fire('input');
  await button(root, 'Find memories').fire('click');
  let q = new URL(calls.at(-1).path, 'http://local').searchParams;
  assert.equal(q.get('valid_at'), new Date(dates[0].value).toISOString());
  assert.equal(q.get('known_at'), new Date(dates[1].value).toISOString());
  await button(root, 'Current belief about selected world time').fire('click');
  q = new URL(calls.at(-1).path, 'http://local').searchParams;
  assert(q.has('valid_at')); assert(!q.has('known_at'));
  assert(root.textContent.includes('Supporting evidence (1)'));
  assert(root.textContent.includes('Connections needing review (1)'));
  assert(root.querySelectorAll('a').some(a => a.attributes.href === '#/memory/old'));
  assert(root.querySelectorAll('a').some(a => a.attributes.href === '#/transitions/correction'));
});

test('state lookup errors clear stale data and focused memory loads the real scope', async () => {
  let fail = false;
  const { root, calls } = setup(({ path }) => path.includes('/memories/') ? memory : fail ? { error: 'state failed' } : { current: [memory] });
  await inspector(root, ['m']);
  const query = new URL(calls.at(-1).path, 'http://local').searchParams;
  assert.equal(query.get('user_id'), 'u'); assert.equal(query.get('agent_id'), 'a'); assert.equal(query.get('target_id'), 'm');
  fail = true; await button(root, 'Find memories').fire('click');
  assert(root.textContent.includes('state failed'));
  assert(!root.textContent.includes('What is known (1)'));
});

test('transition lookup encodes exact keys and distinguishes unavailable historical fields', async () => {
  let missing = false;
  const { root, calls } = setup(() => missing ? { error: 'not found' } :
    { status: 'failed', mode: 'shadow', error: 'validation_failed', validation_error: { code: 'unknown_operation' }, plan: {} });
  await transitions(root, ['key/with spaces']);
  assert(calls[0].path.endsWith('key%2Fwith%20spaces'));
  assert(root.textContent.includes('unknown_operation'));
  assert(root.textContent.includes('Not recorded in this trace'));
  assert(root.textContent.includes('duration not recorded'));
  missing = true; await button(root, 'Load result').fire('click');
  assert(root.textContent.includes('not found')); assert(!root.textContent.includes('unknown_operation'));
  labelled(root, 'Operation key').value = '';
  await button(root, 'Load result').fire('click');
  assert.equal(calls.length, 2); assert(root.textContent.includes('Enter an operation key'));
});

test('tenant/key switch and navigation discard pending results', async () => {
  let finish;
  const { root } = setup(({ path }) => {
    if (path.includes('/config')) return config();
    if (path.includes('/state')) return {};
    return new Promise(resolve => { finish = resolve; });
  });
  await playground(root, []); await edit(root);
  const pending = preview(root);
  lib.state.tenant = 'b'; lib.state.key = 'other-credential';
  finish(receipt('proposed')); await pending;
  assert(!root.textContent.includes('Preview only — nothing saved'));
  assert(button(root, 'Commit / retry same operation').disabled);
  root.mounted = false;
  assert.equal(ui.workspaceSession(root)(), false);
});

test('explorer links real focused memories into knowledge state and transition playground', async () => {
  const { root } = setup(() => ({ memories: [memory, { ...memory, id: 'old' }], inferred_links: [],
    relationships: [{ source_id: 'm', target_id: 'old', relation: 'corrects', origin: 'validated', operation_key: 'correction' }], more: false }));
  await explorer(root, ['m']);
  const hrefs = root.querySelectorAll('a').map(a => a.attributes.href);
  assert(hrefs.includes('#/knowledge-state/m')); assert(hrefs.includes('#/intelligence/m'));
  assert(hrefs.includes('#/transitions/correction'));
});

test('identical source and extraction cards expose their different provenance without hiding records', async () => {
  const source = { ...memory, id: 'source', source: { kind: 'explicit' } };
  const extracted = { ...memory, id: 'extracted', source: { kind: 'knowledge_enriched', derived_by: 'ollama/qwen3:0.6b' },
    metadata: { enrichment: { source_id: 'source', review: 'unreviewed' } } };
  const { root, calls } = setup(() => ({ memories: [source, extracted], relationships: [], more: false }));
  await explorer(root, []);
  const cards = root.querySelectorAll('button').filter(b => b.attributes.class?.startsWith('memory-tile'));
  assert.equal(cards.length, 2);
  assert(cards[0].textContent.includes('Original input'));
  assert(cards[1].textContent.includes('Model extraction · unreviewed'));
  await cards[1].fire('click');
  assert(root.textContent.includes('not a second manual save'));
  assert(root.textContent.includes('ollama/qwen3:0.6b'));
  assert(root.querySelectorAll('a').some(a => a.attributes.href === '#/memory/source' && a.textContent === 'Read original input'));
  assert(calls.every(c => c.method === 'GET'));
});

test('explorer does not label explicit structured assertions as model extractions', async () => {
  const { root } = setup(() => ({ memories: [{ ...memory, source: { kind: 'explicit' },
    metadata: { assertion: { subject: 'Atlas', predicate: 'uses', object: 'Redis' } } }], relationships: [], more: false }));
  await explorer(root, ['m']);
  assert(root.textContent.includes('Saved assertion'));
  assert(!root.textContent.includes('Model extraction'));
  assert(!root.textContent.includes('not a second manual save'));
});

test('memory inspector uses no empty panel and supports opening, focusing and closing without writes', async () => {
  const { root, calls } = setup(() => ({ memories: [memory], relationships: [], more: false }));
  await explorer(root, []);
  const detail = labelled(root, 'Selected memory');
  assert(detail.hidden);
  assert(!root.textContent.includes('Choose a memory'));
  const card = root.querySelectorAll('button').find(b => b.attributes.class?.startsWith('memory-tile'));
  await card.fire('click');
  assert(!detail.hidden);
  assert.equal(document.activeElement, detail);
  assert(detail.parent.attributes.class.includes('has-selection'));
  await button(root, 'Close details').fire('click');
  assert(detail.hidden);
  assert(!detail.parent.attributes.class.includes('has-selection'));
  assert.equal(document.activeElement, root.querySelectorAll('button').find(b => b.attributes.class?.startsWith('memory-tile')));
  assert(calls.every(c => c.method === 'GET'));
});

test('explorer deletion requires confirmation, deletes the selected record and refreshes without refetching deleted focus', async () => {
  let deleted = false, confirmed = false;
  globalThis.confirm = () => confirmed;
  toast.replaceChildren();
  const { root, calls } = setup(({ method }) => {
    if (method === 'DELETE') { deleted = true; return { status: 'deleted' }; }
    return { memories: deleted ? [] : [memory], relationships: [], more: false };
  });
  await explorer(root, ['m']);
  await button(root, 'Delete memory').fire('click');
  assert(!calls.some(c => c.method === 'DELETE'));
  confirmed = true;
  await button(root, 'Delete memory').fire('click');
  assert.equal(calls.filter(c => c.method === 'DELETE').length, 1);
  assert.equal(calls.find(c => c.method === 'DELETE').path, '/v1/memories/m');
  assert(labelled(root, 'Selected memory').hidden);
  assert(root.textContent.includes('0 memories shown'));
  assert(toast.textContent.includes('Memory deleted'));
  assert(!calls.some(c => c.method === 'GET' && c.path === '/v1/memories/m'));
});

test('explorer deletion failures remain visible and stale tenant controls cannot delete', async () => {
  globalThis.confirm = () => true;
  toast.replaceChildren();
  const { root, calls } = setup(({ method }) => method === 'DELETE'
    ? { error: 'Deletion forbidden' } : { memories: [memory], relationships: [], more: false });
  await explorer(root, ['m']);
  await button(root, 'Delete memory').fire('click');
  assert(toast.textContent.includes('Deletion forbidden'));
  assert(!toast.textContent.includes('Memory deleted'));
  assert(!labelled(root, 'Selected memory').hidden);
  assert(!button(root, 'Delete memory').disabled);
  lib.state.tenant = 'other';
  await button(root, 'Delete memory').fire('click');
  assert.equal(calls.filter(c => c.method === 'DELETE').length, 1);
});

test('explorer prevents duplicate deletion and discards results after navigation', async () => {
  let finishDelete;
  globalThis.confirm = () => true;
  toast.replaceChildren();
  const { root, calls } = setup(({ method }) => method === 'DELETE'
    ? new Promise(resolve => { finishDelete = resolve; })
    : { memories: [memory], relationships: [], more: false });
  await explorer(root, ['m']);
  const oldButton = button(root, 'Delete memory');
  const pending = oldButton.fire('click');
  assert(button(root, 'Deleting memory…').disabled);
  await oldButton.fire('click');
  assert.equal(calls.filter(c => c.method === 'DELETE').length, 1);
  assert.equal(calls.find(c => c.method === 'DELETE').headers['X-Tenant'], 'a');
  root.mounted = false;
  root.replaceChildren(new Element('div'));
  finishDelete({ status: 'deleted' });
  await pending;
  assert.equal(toast.textContent, '');
  assert.equal(root.children.length, 1);
  assert.equal(calls.filter(c => c.method === 'GET').length, 1);
});

test('document imports use two upload lanes, real status and authenticated scope', async () => {
  const pending = [], uploads = [];
  const { root, calls } = setup(request => {
    if (request.method === 'POST') {
      uploads.push(request);
      return new Promise(resolve => pending.push(() => resolve({
        id: request.body.name, name: request.body.name, status: 'ingested', pages: 1, memories: ['chunk-' + request.body.name],
      })));
    }
    return { documents: [] };
  });
  const project = new Element('input'); project.value = 'project';
  root.append(documentImport(root, project));
  const input = labelled(root, 'Documents to import');
  input.files = ['one.txt', 'two.md', 'three.docx'].map(name => ({ name, size: 5, arrayBuffer: async () => new TextEncoder().encode('hello').buffer }));
  const upload = button(root, 'Import documents');
  const work = upload.fire('click');
  await new Promise(setImmediate);
  assert.equal(uploads.length, 2);
  assert(upload.disabled);
  assert(uploads.every(r => r.headers['X-Tenant'] === 'a' && r.body.user_id === 'project' && atob(r.body.bytes) === 'hello'));
  pending.shift()();
  await new Promise(setImmediate);
  assert.equal(uploads.length, 3);
  pending.splice(0).forEach(finish => finish());
  await work;
  assert(!upload.disabled);
  assert(root.textContent.includes('3 document(s) accepted'));
  assert(root.textContent.includes('source-linked chunks'));
  assert.equal(calls.filter(r => r.method === 'POST').length, 3);
  await button(root, 'Refresh imports').fire('click');
  assert(root.textContent.includes('No recent imports'));
});

test('multimodal imports send real media bytes and expose model-generated provenance', async () => {
  const { root, calls } = setup(({ method }) => method === 'POST'
    ? { id: 'media', name: 'photo.png', status: 'ingested', kind: 'media', model: 'ollama/lfm3-omni:latest', unreviewed: true, pages: 1, memories: ['interpretation'] }
    : { documents: [] });
  lib.state.info = { capabilities: { media_interpretation: 'ollama/lfm3-omni:latest' } };
  const project = new Element('input'); project.value = 'atlas';
  root.append(documentImport(root, project, { media: true }));
  labelled(root, 'Images, audio or video to interpret').files = [{ name: 'photo.png', size: 5, arrayBuffer: async () => new TextEncoder().encode('image').buffer }];
  await button(root, 'Interpret media').fire('click');
  const upload = calls.find(c => c.method === 'POST');
  assert.equal(upload.path, '/v1/media');
  assert.equal(atob(upload.body.bytes), 'image');
  assert.equal(upload.body.user_id, 'atlas');
  assert(root.textContent.includes('Unreviewed model interpretation'));
  assert(root.textContent.includes('lfm3-omni:latest'));
  assert(root.textContent.includes('not a verified observation'));
  assert(root.querySelectorAll('a').some(a => a.attributes.href === '#/memory/interpretation'));
});

test('multimodal controls remain disabled without a configured interpreter', async () => {
  const { root, calls } = setup(() => ({}));
  root.append(documentImport(root, new Element('input'), { media: true }));
  assert(button(root, 'Interpret media').disabled);
  assert(root.textContent.includes('Media interpretation is disabled'));
  await button(root, 'Interpret media').fire('click');
  assert.equal(calls.length, 0);
});

test('media memory inspector labels interpretation separately from verified original evidence', async () => {
  const m = { ...memory, source: { kind: 'media_interpreted', trust: 'low', derived_by: 'ollama/lfm3-omni:latest' },
    metadata: { filename: 'photo.png', interpretation: 'unreviewed' } };
  const { root } = setup(() => ({ memories: [m], relationships: [], more: false }));
  await explorer(root, ['m']);
  assert(root.textContent.includes('Media interpretation · unreviewed'));
  assert(root.textContent.includes('not verified evidence'));
  assert(root.textContent.includes('photo.png'));
  assert(root.textContent.includes('ollama/lfm3-omni:latest'));
});

test('document imports validate bounds and never upload stale tenant files', async () => {
  const { root, calls } = setup(() => ({}));
  root.append(documentImport(root, new Element('input')));
  const input = labelled(root, 'Documents to import');
  await button(root, 'Import documents').fire('click');
  assert(root.textContent.includes('Choose one or more'));
  input.files = [{ name: 'large.pdf', size: 21 * 1024 * 1024 }];
  await button(root, 'Import documents').fire('click');
  assert(root.textContent.includes('20 MiB'));
  input.files = [{ name: 'bad.exe', size: 10 }];
  await button(root, 'Import documents').fire('click');
  assert(root.textContent.includes('Supported formats'));
  let finish;
  input.files = [{ name: 'one.txt', size: 5, arrayBuffer: () => new Promise(resolve => { finish = resolve; }) }];
  const work = button(root, 'Import documents').fire('click');
  lib.state.tenant = 'other';
  finish(new ArrayBuffer(5));
  await work;
  assert.equal(calls.length, 0);
});

test('document upload failures and durable processing failures stay explicit', async () => {
  const { root } = setup(({ method }) => method === 'POST' ? { error: 'parser disabled' }
    : { documents: [{ id: 'failed', name: 'scan.pdf', status: 'failed', memories: [], error: 'install tesseract-ocr' }] });
  root.append(documentImport(root, new Element('input')));
  labelled(root, 'Documents to import').files = [{ name: 'one.txt', size: 5, arrayBuffer: async () => new ArrayBuffer(5) }];
  await button(root, 'Import documents').fire('click');
  assert(root.textContent.includes('parser disabled'));
  assert(root.textContent.includes('0 document(s) accepted; 1 upload(s) failed'));
  await button(root, 'Refresh imports').fire('click');
  assert(root.textContent.includes('install tesseract-ocr'));
  assert(root.textContent.includes('Import failed'));
});

test('workspace browse, timeline and memory history accept legacy null collections', async () => {
  toast.replaceChildren();
  for (const view of [explorer, timelinePage]) {
    const { root } = setup(() => ({ memories: null, relationships: null, inferred_links: null }));
    await view(root, []);
    assert(root.textContent.includes(view === explorer ? '0 memories shown' : 'No memory events'));
  }
  const { root } = setup(({ path }) => path.endsWith('/versions') ? { versions: null }
    : path.endsWith('/history') ? { events: null } : memory);
  await memoryDetail(root, ['m']);
  assert(root.textContent.includes('Version timeline'));
  assert.equal(toast.textContent, '');
});

test('entity graph accepts null entities, neighbors and memories from older servers', async t => {
  const createElement = document.createElement;
  document.createElement = tag => {
    const el = createElement(tag);
    if (tag === 'canvas') {
      el.clientWidth = 640;
      el.getContext = () => ({ clearRect() {}, beginPath() {}, arc() {}, fill() {}, fillText() {}, moveTo() {}, lineTo() {}, stroke() {} });
    }
    return el;
  };
  const ratio = globalThis.devicePixelRatio;
  globalThis.devicePixelRatio = 1;
  t.after(() => { document.createElement = createElement; globalThis.devicePixelRatio = ratio; });
  toast.replaceChildren();
  const { root } = setup(({ path }) => path.startsWith('/v1/entities?')
    ? { entities: null } : { entity: { name: 'Atlas', mentions: 0 }, related: null, memories: null });
  await graphPage(root, ['Atlas']);
  assert(root.textContent.includes('No entities found.'));
  assert(root.textContent.includes('No current readable memories'));
  assert.equal(toast.textContent, '');
});

test('search leads with natural language while preserving advanced filters and real retrieval', async () => {
  const { root, calls } = setup(() => ({ trace: [], hits: [], took_ns: 1000 }));
  await searchPage(root);
  const query = root.querySelectorAll('label').find(l => l.textContent.includes('What do you want to know?')).querySelector('input');
  query.value = 'project conventions'; await query.fire('input');
  const options = root.querySelectorAll('details').find(d => d.textContent.includes('Advanced search options'));
  assert(options);
  assert.equal(options.attributes.open, undefined);
  const scope = options.querySelectorAll('label').find(l => l.textContent.startsWith('user_id')).querySelector('input');
  scope.value = 'project-a'; await scope.fire('input');
  await button(root, 'Search').fire('click');
  assert.equal(calls[0].path, '/v1/search');
  assert.equal(calls[0].body.query, 'project conventions');
  assert.equal(calls[0].body.user_id, 'project-a');
  assert(root.textContent.includes('0 hits'));
  assert(!toast.textContent.includes('Cannot read'));
});

test('search displays readable evidence before collapsed ranking diagnostics', async () => {
  const { root } = setup(() => ({ trace: [], hits: [{ memory: { ...memory, id: 'memory/encoded', text: 'PostgreSQL stores project knowledge.' },
    score: 0.8 }], took_ns: 1000 }));
  await searchPage(root);
  await button(root, 'Search').fire('click');
  const results = labelled(root, 'Search results');
  assert(results.textContent.includes('PostgreSQL stores project knowledge.'));
  assert(results.querySelectorAll('a').some(a => a.attributes.href === '#/memory/memory%2Fencoded'));
  const diagnostics = root.querySelectorAll('details').find(d => d.textContent.includes('Retrieval diagnostics & scores'));
  assert(diagnostics);
  assert.equal(diagnostics.attributes.open, undefined);
  assert(results.parent.children.indexOf(results) < results.parent.children.indexOf(diagnostics));
});

test('empty search and context handle null collections from older servers without errors', async () => {
  const { root } = setup(({ path }) => path === '/v1/search'
    ? { hits: null, trace: null, took_ns: 1000 }
    : { context: { profile: null, scenes: null, facts: null, skills: null, took_ns: 1000 }, rendered: '' });
  toast.replaceChildren();
  await searchPage(root);
  await button(root, 'Search').fire('click');
  assert(root.textContent.includes('0 hits'));
  assert(root.textContent.includes('No matching memories.'));
  await button(root, 'Preview Context()').fire('click');
  assert(root.textContent.includes('0 profile · 0 scenes · 0 facts · 0 skills'));
  assert.equal(toast.textContent, '');
});

test('stale controls cannot submit old input under a new tenant or credential', async () => {
  const { root, calls } = setup(({ path }) => path.includes('/config') ? config() :
    path.includes('/state') ? {} : receipt('proposed'));
  await playground(root, []); await edit(root); await preview(root); await confirm(root);
  lib.state.tenant = 'other'; lib.state.key = 'other-key';
  const count = calls.length;
  await commit(root);
  await button(root, 'Save tenant intelligence configuration').fire('click');
  await preview(root);
  assert.equal(calls.length, count);
});

test('guided form sends real input without JSON and gates a save behind review', async () => {
  const { root, calls } = setup(({ path }) => path.includes('/config') ? config() :
    path.includes('/state') ? {} : receipt('proposed'));
  await playground(root, []);
  assert(labelled(root, 'Knowledge input JSON').disabled);
  assert(root.querySelectorAll('details').every(d => d.attributes.open === undefined));
  assert(root.querySelectorAll('section').find(section => section.textContent.includes('Keep this result?')).hidden);
  await preview(root);
  assert(root.querySelectorAll('section').find(section => section.textContent.includes('Keep this result?')).hidden);
  assert(root.textContent.includes('Write something to remember first.'));
  assert.equal(calls.filter(c => c.method === 'POST').length, 0);
  const text = labelled(root, 'What should I remember?'), project = labelled(root, 'Project or user');
  text.value = 'Use PostgreSQL in production'; await text.fire('input');
  project.value = 'my-project'; await project.fire('input');
  await preview(root);
  assert(!root.querySelectorAll('section').find(section => section.textContent.includes('Keep this result?')).hidden);
  const request = calls.find(c => c.path.endsWith('/evaluate'));
  assert.equal(request.body.input.text, text.value);
  assert.equal(request.body.input.scope.user_id, 'my-project');
  assert.equal(request.body.input.trust, 'high');
  assert(button(root, 'Commit / retry same operation').disabled);
  await confirm(root);
  assert.equal(button(root, 'Commit / retry same operation').textContent, 'Save to memory');
  await preview(root);
  assert(!labelled(root, 'Confirm durable commit').checked);
  assert(button(root, 'Commit / retry same operation').disabled);
});

test('simple correction preserves target and scope; advanced JSON is explicitly opt-in', async () => {
  const { root, calls } = setup(({ path }) => path.includes('/config') ? config() :
    path.includes('/memories/') ? { ...memory, slot: 'project.db', visibility: 'restricted', grants: ['team:dev'] } :
      path.includes('/state') ? {} : receipt('proposed'));
  await playground(root, ['m']);
  const text = labelled(root, 'What should I remember?');
  text.value = 'Updated fact'; await text.fire('input');
  await preview(root);
  const simple = calls.find(c => c.path.endsWith('/evaluate')).body.input;
  assert.equal(simple.target_id, 'm'); assert.equal(simple.intent, 'CORRECT');
  assert.equal(simple.scope.agent_id, 'a'); assert.equal(simple.visibility, 'restricted');
  assert.deepEqual(simple.grants, ['team:dev']);
  await edit(root, { text: 'Advanced fact', intent: 'BRANCH', evidence: [{ episode_id: 'episode' }] });
  assert(text.disabled);
  await preview(root);
  assert.equal(calls.at(-1).body.input.intent, 'BRANCH');
  const toggle = labelled(root, 'Use JSON instead of the form');
  toggle.checked = false; await toggle.fire('change');
  assert(!text.disabled); assert(labelled(root, 'Knowledge input JSON').disabled);
  await preview(root);
  assert.equal(calls.at(-1).body.input.text, 'Updated fact');
  assert.equal(calls.at(-1).body.input.intent, 'CORRECT');
});

test('plain-language outcomes never present shadow or connection-only results as saved memories', () => {
  assert.equal(ui.transitionOutcome(receipt('proposed')).title, 'Preview only — nothing saved');
  assert.equal(ui.transitionOutcome(receipt('shadow')).title, 'Test result recorded — memories unchanged');
  assert.equal(ui.transitionOutcome(receipt('committed')).title, '1 memory change saved');
  const relate = { ...receipt('committed'), changes: [], plan: { operations: [{ type: 'RELATE', reason_code: 'supported_by_evidence' }] } };
  assert.equal(ui.transitionOutcome(relate).title, 'Result recorded — no memory versions changed');
  const rendered = ui.renderTransition(relate);
  assert(rendered.textContent.includes('Connect existing memories'));
  assert(rendered.textContent.includes('may have changed connections only'));
  assert(rendered.querySelectorAll('details').every(d => d.attributes.open === undefined));
  assert.equal(ui.transitionOutcome({ status: 'admission:SKIP' }).title, 'Recorded result');
});

test('memory browser is cards-first with graph and diagnostics collapsed; history is optional', async () => {
  const { root } = setup(() => ({ memories: [memory, { ...memory, id: 'old', text: 'Old assertion', invalidated_at: '2026-01-01T00:00:00Z' }],
    relationships: [], inferred_links: [], more: false }));
  await explorer(root, []);
  const tiles = () => root.querySelectorAll('button').filter(b => (b.attributes.class || '').startsWith('memory-tile'));
  assert.equal(tiles().length, 1);
  const panels = root.querySelectorAll('details');
  assert(panels.some(p => p.textContent.includes('Explore connections as a graph')));
  assert(panels.every(p => p.attributes.open === undefined));
  const toggle = labelled(root, 'Include older versions');
  toggle.checked = true; await toggle.fire('change');
  assert.equal(tiles().length, 2);
  await tiles()[1].fire('click');
  assert(root.textContent.includes('Old assertion'));
  assert(root.querySelectorAll('a').some(a => a.attributes.href === '#/intelligence/old'));
});

test('planner and real embedding coverage are visible without advanced controls and can refresh', async () => {
  let have = 1;
  const { root, calls } = setup(() => config(), ({ path }) => path === '/_dev/api/info'
    ? { capabilities: { embedder: 'ollama/nomic-embed-text@1' },
      knowledge: { effective: { mode: 'enforced', provider: 'ollama', model: 'qwen3:1.7b' } } }
    : { have, total: 3 });
  await playground(root, []);
  assert(root.textContent.includes('Decision planner'));
  assert(root.textContent.includes('qwen3:1.7b'));
  assert(root.textContent.includes('Live — confirmed saves are enabled'));
  assert(root.textContent.includes('Embedding system'));
  assert(root.textContent.includes('1 / 3 memories embedded'));
  assert(calls.some(c => c.path === '/v1/vectors/coverage?model=ollama%2Fnomic-embed-text%401'));
  have = 3;
  await button(root, 'Refresh model status').fire('click');
  assert(root.textContent.includes('3 / 3 memories embedded — coverage complete'));
  assert(root.querySelectorAll('details').every(d => d.attributes.open === undefined));
});

test('status explicitly distinguishes missing embeddings, test vectors and coverage failure', async () => {
  const missing = ui.renderSystems({ capabilities: {}, knowledge: config() });
  assert(missing.textContent.includes('Semantic embeddings are off'));
  const hash = ui.renderSystems({ capabilities: { embedder: 'hash@1' }, knowledge: config() }, { have: 2, total: 2 });
  assert(hash.textContent.includes('Test hash vectors only — not semantic'));
  const { root } = setup(() => config(), ({ path }) => path === '/_dev/api/info'
    ? { capabilities: { embedder: 'local-embedder' }, knowledge: config() } : { error: 'coverage request failed' });
  await playground(root, []);
  assert(root.textContent.includes('Embedding coverage unavailable: coverage request failed'));
  assert(!root.textContent.includes('coverage complete'));
});

test('model status authentication failure is explicit', async () => {
  const { root } = setup(() => config(), () => ({ error: 'missing or invalid API key' }));
  await playground(root, []);
  assert(root.textContent.includes('Model status unavailable: missing or invalid API key'));
});

test('fast save persists directly and displays grounded extraction without preview or confirmation', async () => {
  const { root, calls } = setup(({ path, body }) => {
    if (path.endsWith('/config')) return { ...config(), effective: { ...config().effective, background_enrichment: true } };
    if (path === '/v1/facts') {
      assert.equal(body.facts[0].text, 'Atlas uses Redis.');
      return { changes: [{ action: 'add', memory }] };
    }
    if (path.includes('/enrichment/')) return { status: 'completed', source_id: 'm', extraction: {
      model: 'local-cpu', facts: [{ quote: 'Atlas uses Redis.', subject: 'Atlas', predicate: 'uses', object: 'Redis', type: 'fact' }] },
      changes: [{ memory: { id: 'derived' } }], relations: [] };
    throw new Error(path);
  });
  await playground(root, []);
  const text = labelled(root, 'What should I remember?'); text.value = 'Atlas uses Redis.'; await text.fire('input');
  await button(root, 'Save now').fire('click');
  assert(root.textContent.includes('Original saved'));
  assert(root.textContent.includes('Extraction complete'));
  assert(root.textContent.includes('Atlas · uses · Redis'));
  assert(root.textContent.includes('unreviewed'));
  assert(!calls.some(c => /\/evaluate$|\/commit$/.test(c.path)));
  assert(text.disabled);
  assert(button(root, 'Preview validated transition').disabled);
  await button(root, 'Start over').fire('click');
  assert(!text.disabled);
});

test('confirmed save unlocks controls without waiting for extraction status or embedding coverage', async () => {
  let saved = false, finishStatus, finishInfo;
  const { root, calls } = setup(({ path }) => {
    if (path.endsWith('/config')) return { ...config(), effective: { ...config().effective, background_enrichment: true } };
    if (path === '/v1/facts') { saved = true; return { changes: [{ action: 'add', memory }] }; }
    if (path.includes('/enrichment/')) return new Promise(resolve => { finishStatus = resolve; });
    throw new Error(path);
  }, () => saved ? new Promise(resolve => { finishInfo = resolve; }) : { capabilities: {} });
  await playground(root, []);
  const text = labelled(root, 'What should I remember?'); text.value = 'Atlas uses Redis.'; await text.fire('input');
  await button(root, 'Save now').fire('click');
  assert(root.textContent.includes('Original saved'));
  assert(button(root, 'Saved').disabled);
  assert(!button(root, 'Start over').disabled);
  assert(!root.textContent.includes('Working with the backend'));
  await button(root, 'Start over').fire('click');
  assert(!text.disabled);
  assert(!button(root, 'Save now').disabled);
  finishStatus({ status: 'completed', source_id: 'm' });
  finishInfo({ capabilities: {} });
  await new Promise(setImmediate);
  assert(!root.textContent.includes('Extraction complete'));
  assert.equal(calls.filter(c => c.path === '/v1/facts').length, 1);
});

test('fast save retries the identical body after lost response and reports status failures separately', async () => {
  let attempts = 0;
  const bodies = [];
  const { root } = setup(({ path, body }) => {
    if (path.endsWith('/config')) return { ...config(), effective: { ...config().effective, background_enrichment: true } };
    if (path === '/v1/facts') {
      bodies.push(body);
      if (attempts++ === 0) return { error: 'connection lost' };
      return { changes: [{ action: 'add', memory }] };
    }
    if (path.includes('/enrichment/')) return { error: 'status unavailable' };
    throw new Error(path);
  });
  await playground(root, []);
  const text = labelled(root, 'What should I remember?'); text.value = 'Atlas uses Redis.'; await text.fire('input');
  await button(root, 'Save now').fire('click');
  assert(root.textContent.includes('Save not confirmed'));
  assert(text.disabled);
  await button(root, 'Retry / retrieve saved memory').fire('click');
  assert.deepEqual(bodies[0], bodies[1]);
  assert(root.textContent.includes('Original saved'));
  assert(root.textContent.includes('Extraction status unavailable'));
});

test('enrichment inspector refreshes real status and discards stale tenant responses', async () => {
  let finish;
  const { root } = setup(() => new Promise(resolve => { finish = resolve; }));
  const pending = ui.inspectEnrichment(root, 'm');
  lib.state.tenant = 'different';
  finish({ status: 'completed', extraction: { model: 'secret-model', facts: [] } });
  await pending;
  assert(!root.textContent.includes('secret-model'));
});

test('enrichment status refresh moves through actual queued, processing and completed states', async () => {
  let index = 0;
  const { root, calls } = setup(() => ({ status: ['queued', 'processing', 'completed'][index++] }));
  await ui.inspectEnrichment(root, 'm');
  assert(root.textContent.includes('Waiting for extraction'));
  await button(root, 'Refresh extraction status').fire('click');
  assert(root.textContent.includes('Extracting knowledge'));
  await button(root, 'Refresh extraction status').fire('click');
  assert(root.textContent.includes('Extraction complete'));
  assert.equal(calls.length, 3);
  root.mounted = false;
});

test('memory detail combines real extraction results with original history', async () => {
  const { root } = setup(({ path }) => {
    if (path === '/v1/memories/m') return memory;
    if (path.endsWith('/versions')) return { versions: [memory] };
    if (path.endsWith('/history')) return { events: [] };
    if (path.includes('/enrichment/')) return { status: 'completed', changes: [{ memory: { id: 'derived' } }] };
    throw new Error(path);
  });
  lib.state.info = { capabilities: { background_enrichment: true } };
  await memoryDetail(root, ['m']);
  assert(root.textContent.includes('Extraction complete'));
  assert(root.textContent.includes('Version timeline'));
  assert(root.textContent.includes('History'));
  assert(root.querySelectorAll('a').some(a => a.attributes.href === '#/memory/derived'));
});

test('enrichment results distinguish shadow, failed processing and potential conflicts', () => {
  assert(ui.renderEnrichment({ status: 'shadow' }).textContent.includes('not applied'));
  assert(ui.renderEnrichment({ status: 'failed', error: 'model timeout' }).textContent.includes('model timeout'));
  assert(ui.renderEnrichment({ status: 'completed', relations: [{ source_id: 'a', target_id: 'b', relation: 'contradicts' }] }).textContent.includes('potential conflict, needs review'));
});
