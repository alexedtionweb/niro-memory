// Console shell: navigation, connection settings and a hash router.
import { state, call, h, toast } from './lib.js';

const pages = [
  ['Data', [
    ['tenants', 'Tenants & settings'],
    ['memories', 'Memory browser'],
    ['conversations', 'Conversations'],
    ['sql', 'SQL console'],
  ]],
  ['Retrieval', [
    ['search', 'Search playground'],
    ['embeddings', 'Embeddings'],
    ['graph', 'Entity graph'],
  ]],
  ['Processing', [
    ['rules', 'Rules'],
    ['add', 'Add playground'],
    ['jobs', 'Jobs'],
    ['metrics', 'Metrics'],
  ]],
];

function nav() {
  const current = location.hash.slice(2).split(/[/?]/)[0] || 'tenants';
  document.getElementById('nav').replaceChildren(...pages.flatMap(([group, items]) => [
    h('div', { class: 'group' }, group),
    ...items.map(([id, label]) => h('a', { href: '#/' + id, class: current === id ? 'active' : '' }, label)),
  ]));
}

async function refreshInfo() {
  try {
    state.info = await call('GET', '/_dev/api/info');
    const c = state.info.capabilities;
    document.getElementById('caps').textContent =
      `lexical: ${c.bm25 || (c.lexical ? 'yes' : 'no')} · entities: ${c.entities ? 'yes' : 'no'} · vector: ${c.vector ? (c.external_vector_index ? 'external' : 'store') : 'no'}` +
      ` · embedder: ${c.embedder || 'none'} · rules: ${c.rules || 'none'}`;
  } catch (e) {
    state.info = null;
    document.getElementById('caps').textContent = 'not connected: ' + e.message;
  }
}

async function route() {
  nav();
  document.getElementById('tenant').value = state.tenant;
  const [name, ...params] = location.hash.slice(2).split('?')[0].split('/');
  const id = name || 'tenants';
  const main = document.getElementById('main');
  main.replaceChildren(h('p', { class: 'muted' }, 'Loading…'));
  try {
    const mod = await import('./pages/' + (id === 'memory' ? 'memory' : id) + '.js');
    await mod.default(main, params.map(decodeURIComponent));
  } catch (e) {
    main.replaceChildren(h('h1', null, 'Error'), h('pre', null, e.stack || e.message));
  }
}

function connection() {
  const t = document.getElementById('tenant');
  const k = document.getElementById('key');
  t.value = state.tenant;
  k.value = state.key;
  const apply = async () => {
    state.tenant = t.value.trim() || 'dev';
    state.key = k.value.trim();
    await refreshInfo();
    route();
  };
  t.addEventListener('change', apply);
  k.addEventListener('change', apply);
}

window.addEventListener('hashchange', route);
window.addEventListener('niromem:tenant', async () => {
  document.getElementById('tenant').value = state.tenant;
  await refreshInfo();
  toast('Switched to tenant ' + state.tenant);
});
connection();
refreshInfo().then(route);
