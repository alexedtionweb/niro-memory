import { state, call, h, toast, requestHeaders } from './lib.js';
import { screens, resolveRoute } from './routes.js';

const main = document.getElementById('main');
let generation = 0, infoGeneration = 0;
const requests = new WeakMap();
const active = (context) => context && context.generation === generation &&
  context.tenant === state.tenant && context.key === state.key;

function nav(screen) {
  document.getElementById('nav').replaceChildren(...screens.map(s =>
    h('a', { href: '#/' + s.id, class: screen === s.id ? 'active' : '',
      'aria-current': screen === s.id ? 'page' : null }, s.label)));
}

async function refreshInfo() {
  const revision = ++infoGeneration, tenant = state.tenant, key = state.key;
  try {
    const info = await call('GET', '/_dev/api/info');
    if (revision !== infoGeneration || tenant !== state.tenant || key !== state.key) return;
    state.info = info;
    document.getElementById('caps').textContent =
      `Connected · ${info.knowledge?.effective?.model || 'Memory engine'} · ${info.capabilities?.knowledge_mode || 'disabled'}`;
  } catch (e) {
    if (revision !== infoGeneration || tenant !== state.tenant || key !== state.key) return;
    state.info = null;
    document.getElementById('caps').textContent = 'Not connected: ' + e.message;
    document.querySelector('.connection-settings').open = true;
  }
}

function failure(message) {
  main.removeAttribute('aria-busy');
  main.replaceChildren(h('section', { class: 'card', role: 'alert' },
    h('h1', null, 'Unable to open workspace'), h('p', { class: 'error-message' }, message),
    h('button', { onclick: route }, 'Retry')));
}

async function mount(context) {
  if (!active(context)) return;
  const root = main.querySelector('#workspace-content');
  if (!root) { failure('The server returned an invalid workspace fragment.'); return; }
  try {
    const mod = await import('./pages/' + context.route.view + '.js');
    if (!active(context)) return;
    await mod.default(root, context.route.params);
    if (active(context)) main.focus({ preventScroll: true });
  } catch (e) {
    if (active(context)) root.replaceChildren(h('p', { class: 'error-message', role: 'alert' }, e.message));
  }
}

async function route() {
  const revision = ++generation;
  let target;
  try { target = resolveRoute(location.hash); } catch (e) { nav(''); failure(e.message); return; }
  history.replaceState(null, '', target.hash);
  nav(target.screen);
  document.getElementById('tenant').value = state.tenant;
  const context = { generation: revision, tenant: state.tenant, key: state.key, route: target };
  main.replaceChildren(h('p', { class: 'muted', role: 'status' }, 'Loading workspace…'));
  main.setAttribute('aria-busy', 'true');
  // Independent sources keep rapid navigation out of HTMX's per-element queue.
  const source = h('span', { hidden: true });
  main.append(source);
  const before = (event) => {
    requests.set(event.detail.xhr, context);
  };
  source.addEventListener('htmx:beforeRequest', before);
  try {
    await window.htmx.ajax('GET', target.path, {
      source, target: main, swap: 'innerHTML', headers: requestHeaders(false),
    });
  } catch (e) {
    if (active(context) && !context.failed) failure('Connection failed. Check that the server is running and retry.');
  } finally {
    source.removeEventListener('htmx:beforeRequest', before);
    source.remove();
    if (active(context)) main.removeAttribute('aria-busy');
  }
}

main.addEventListener('htmx:beforeSwap', event => {
  const context = requests.get(event.detail.xhr);
  if (!active(context)) { event.detail.shouldSwap = false; return; }
  if (event.detail.isError) {
    context.failed = true;
    failure(event.detail.xhr.status === 401 || event.detail.xhr.status === 403
      ? 'An admin identity is required. Check the API key in Connection settings.'
      : `Workspace request failed (HTTP ${event.detail.xhr.status}).`);
    event.detail.shouldSwap = false;
  }
});
main.addEventListener('htmx:afterSwap', event => { mount(requests.get(event.detail.xhr)); });

async function connect() {
  ++generation;
  main.replaceChildren(h('p', { role: 'status' }, 'Connecting…'));
  await refreshInfo();
  await route();
}

const tenant = document.getElementById('tenant'), key = document.getElementById('key');
tenant.value = state.tenant; key.value = state.key;
for (const input of [tenant, key]) input.addEventListener('change', () => {
  state.tenant = tenant.value.trim() || 'dev'; state.key = key.value.trim(); connect();
});
window.addEventListener('hashchange', route);
window.addEventListener('niromem:config', refreshInfo);
window.addEventListener('niromem:tenant', () => {
  tenant.value = state.tenant; toast('Switched to tenant ' + state.tenant); connect();
});
refreshInfo().then(route);
