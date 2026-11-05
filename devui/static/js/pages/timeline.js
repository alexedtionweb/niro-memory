import { call, h, page, badge, date, guard } from '../lib.js';
import { timelineEvents } from '../explorer-model.js';
import { workspaceSession } from '../knowledge-ui.js';

export default async function(root) {
  const active = workspaceSession(root);
  let generation = 0;
  let offset = 0, data, mode = 'recorded';
  const events = h('div', { class: 'timeline-feed' }), count = h('p', { class: 'muted' });
  const user = h('input', { placeholder: 'user_id (optional)', 'aria-label': 'Timeline user scope' });
  const select = h('select', { 'aria-label': 'Timeline clock' },
    h('option', { value: 'recorded' }, 'Knowledge time — when recorded'),
    h('option', { value: 'valid' }, 'World time — when valid'));
  const previous = h('button', { onclick: guard(async () => { offset = Math.max(0, offset - 100); await load(); }) }, 'Previous slice');
  const next = h('button', { onclick: guard(async () => { offset += 100; await load(); }) }, 'Next slice');
  function render() {
    if (!data) return;
    const rows = timelineEvents(data.memories, data.relationships || data.inferred_links, mode);
    count.textContent = `${data.memories.length} memory versions in this slice · ${rows.length} time markers. This is not the full audit log; open a memory for its complete history.`;
    events.replaceChildren(...(rows.length ? rows.map(event => h('article', { class: 'timeline-entry' },
      h('time', { datetime: event.at }, date(event.at)),
      h('div', { class: 'card' }, badge(event.label, event.link ? 'warn' : ''), badge(event.memory.kind),
        h('a', { href: '#/memory/' + event.memory.id }, h('h2', null, event.memory.title || event.memory.text)),
        h('p', { class: 'muted' }, `v${event.memory.version} · ${event.memory.user_id || '(unowned)'} · ${event.memory.source?.kind || ''}`),
        event.link ? h('p', null, `${event.link.relation}: ${event.link.reason}`, h('small', null,
          !event.link.origin || event.link.origin === 'inferred' ? ' · inferred, not a confirmed fact' : ` · ${event.link.origin}`)) : null,
        h('a', { href: '#/explorer/' + event.memory.id }, 'Explore relationships'))))
      : [h('div', { class: 'empty-state' }, 'No memory events in this slice.')]));
  }
  async function load() {
    if (!active()) return;
    const token = ++generation;
    const result = await call('GET', `/v1/explore?all_versions=true&show_expired=true&limit=100&offset=${offset}&user_id=${encodeURIComponent(user.value.trim())}`);
    if (!active() || token !== generation) return;
    data = { ...result, memories: result.memories ?? [], inferred_links: result.inferred_links ?? [] };
    previous.disabled = offset === 0; next.disabled = !data.more; render();
  }
  select.addEventListener('change', () => { mode = select.value; render(); });
  page(root, 'Memory timeline', 'Separate the time a fact became valid from the time the system learned it. Corrections and inferred links remain visible.',
    h('div', { class: 'card row' }, select, user, h('button', { onclick: guard(async () => { offset = 0; await load(); }) }, 'Apply scope / refresh'),
      h('a', { href: '#/explorer' }, 'Back to explorer')),
    count, events, h('div', { class: 'row' }, previous, next));
  await load();
}
