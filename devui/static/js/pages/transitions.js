import { call, h, page } from '../lib.js';
import { renderTransition, workspaceSession } from '../knowledge-ui.js';

export default async function(root, [key]) {
  root.setAttribute('class', 'knowledge-workspace');
  const active = workspaceSession(root);
  const input = h('input', { value: key || '', 'aria-label': 'Operation key', class: 'grow', placeholder: 'Exact operation key' });
  const output = h('div', { 'aria-live': 'polite' }), error = h('p', { role: 'alert', class: 'error-message' });
  let generation = 0;
  async function load() {
    if (!active()) return;
    const operationKey = input.value.trim(), token = ++generation;
    error.textContent = ''; output.replaceChildren();
    if (!operationKey) { error.textContent = 'Enter an operation key.'; return; }
    try {
      const result = await call('GET', '/v1/knowledge/traces/' + encodeURIComponent(operationKey));
      if (active() && token === generation) output.replaceChildren(renderTransition(result));
    } catch (e) {
      if (active() && token === generation) error.textContent = e.message;
    }
  }
  page(root, 'Check a saved result', 'See what happened to one memory attempt: the suggestion, the safety checks and whether anything changed.',
    h('div', { class: 'card' },
      h('p', null, 'Follow “Open this saved result” after trying a memory, or paste its operation key below.'),
      h('div', { class: 'row' }, input, h('button', { onclick: load }, 'Load result')),
      h('details', null, h('summary', null, 'Why do I need a key?'),
        h('p', null, 'The backend looks up one recorded attempt at a time; it does not list all attempts. Previews are not stored. Older records may lack some technical details.'))),
    error, output);
  if (key) await load();
}
