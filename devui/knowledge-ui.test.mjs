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
    const [tag, attr, value] = selector.match(/^([a-z-]+)?(?:\[([^=]+)="([^"]+)"\])?$/).slice(1);
    return this.children.flatMap(child => [
      ...((!tag || child.tag === tag) && (!attr || child.attributes[attr] === value) ? [child] : []),
      ...child.querySelectorAll(selector),
    ]);
  }
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
