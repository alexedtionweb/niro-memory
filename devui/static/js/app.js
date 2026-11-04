// Console shell: navigation, connection settings and a hash router.
import { state, call, h, toast } from './lib.js';

const pages = [
  ['Workspace', [
    ['explorer', 'Memories'],
    ['intelligence', 'Try a memory'],
    ['knowledge-state', 'What is known?'],
    ['timeline', 'Timeline'],
    ['transitions', 'Saved result'],
  ]],
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
  const current = location.hash.slice(2).split(/[/?]/)[0] || 'explorer';
  const sections = pages.map(([group, items]) => [
    h('div', { class: 'group' }, group),
    ...items.map(([id, label]) => h('a', { href: '#/' + id, class: current === id ? 'active' : '' }, label)),
  ]);
  document.getElementById('nav').replaceChildren(...sections[0],
    h('details', { class: 'advanced-nav', open: pages.slice(1).some(([, items]) => items.some(([id]) => id === current)) },
      h('summary', null, 'Developer tools'), ...sections.slice(1).flat()));
}

let infoGeneration = 0;
async function refreshInfo() {
  const generation = ++infoGeneration, tenant = state.tenant, key = state.key;
  const active = () => generation === infoGeneration && state.tenant === tenant && state.key === key;
  try {
    const info = await call('GET', '/_dev/api/info');
    if (!active()) return;
    state.info = info;
    const c = state.info.capabilities;
    document.getElementById('caps').textContent =
      `Connected · ${info.knowledge?.effective?.model || info.knowledge?.effective?.provider || 'Memory engine'} · ` +
      ((info.knowledge?.effective?.background_enrichment
        ? { disabled: 'Extraction paused — saves enabled', shadow: 'Test extraction — originals saved', enforced: 'Live background extraction' }
        : { disabled: 'Analysis paused', shadow: 'Test mode — memories unchanged', enforced: 'Live mode — confirmed changes allowed' })[c.knowledge_mode] || 'Mode unavailable');
  } catch (e) {
    if (!active()) return;
    state.info = null;
    document.getElementById('caps').textContent = 'not connected: ' + e.message;
  }
}

let routeGeneration = 0;
async function route() {
  const generation = ++routeGeneration, tenant = state.tenant, key = state.key;
  nav();
  document.getElementById('tenant').value = state.tenant;
  const [name, ...params] = location.hash.slice(2).split('?')[0].split('/');
  const id = name || 'explorer';
  const main = document.getElementById('main');
  main.replaceChildren(h('p', { class: 'muted' }, 'Loading…'));
  try {
    const mod = await import('./pages/' + (id === 'memory' ? 'memory' : id) + '.js');
    if (generation !== routeGeneration || state.tenant !== tenant || state.key !== key) return;
    const content = h('div');
    main.replaceChildren(content);
    await mod.default(content, params.map(decodeURIComponent));
  } catch (e) {
    if (generation === routeGeneration && state.tenant === tenant && state.key === key)
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
    document.getElementById('main').replaceChildren(h('p', { class: 'muted' }, 'Connecting…'));
    await refreshInfo();
    route();
  };
  t.addEventListener('change', apply);
  k.addEventListener('change', apply);
}

window.addEventListener('hashchange', route);
window.addEventListener('niromem:config', refreshInfo);
window.addEventListener('niromem:tenant', async () => {
  document.getElementById('main').replaceChildren(h('p', { class: 'muted' }, 'Connecting…'));
  document.getElementById('tenant').value = state.tenant;
  await refreshInfo();
  toast('Switched to tenant ' + state.tenant);
  route();
});
connection();
refreshInfo().then(route);
