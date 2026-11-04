import { call, h, page, field, guard } from '../lib.js';
import { statePath, scopedInput, renderState, workspaceSession } from '../knowledge-ui.js';

export default async function(root, [id]) {
  root.setAttribute('class', 'knowledge-workspace');
  const active = workspaceSession(root);
  const input = await scopedInput(id);
  if (!active()) return;
  const model = { user: input.scope.user_id, agent: input.scope.agent_id, run: input.scope.run_id || '',
    target: input.target_id || '', slot: input.slot || '', text: '', subject: '', predicate: '', object: '',
    valid: '', known: '', limit: 16 };
  const output = h('div', { 'aria-live': 'polite' }), error = h('p', { role: 'alert', class: 'error-message' });
  let generation = 0;
  const load = guard(async () => {
    if (!active()) return;
    const token = ++generation;
    output.replaceChildren(h('p', null, 'Loading state…')); error.textContent = '';
    try {
      const request = { input: { scope: { user_id: model.user, agent_id: model.agent, run_id: model.run },
      target_id: model.target, text: model.text, slot: model.slot,
      assertion: { subject: model.subject, predicate: model.predicate, object: model.object } },
      max_candidates: model.limit,
      valid_at: model.valid ? new Date(model.valid).toISOString() : undefined,
      known_at: model.known ? new Date(model.known).toISOString() : undefined };
      const value = await call('GET', statePath(request));
      if (active() && token === generation) output.replaceChildren(renderState(value));
    } catch (e) {
      if (active() && token === generation) { output.replaceChildren(); error.textContent = e.message; }
    }
  });
  page(root, 'What is known?', 'Find memories for a project or topic. Start with today; open the date controls only when you need to explore the past.',
    h('div', { class: 'card' },
      h('div', { class: 'row' }, field('Project or user', model, 'user'), field('Search for a topic', model, 'text'),
        h('button', { class: 'primary', onclick: load }, 'Find memories')),
      h('details', null, h('summary', null, 'Explore a past date'),
        h('p', { class: 'muted' }, 'What was true on a date is different from what we knew on that date. Leave both empty for today.'),
        h('div', { class: 'row' }, field('Valid at (world time)', model, 'valid', { type: 'datetime-local' }),
        field('Known at (belief time)', model, 'known', { type: 'datetime-local' }),
        h('button', { onclick: load }, 'Apply dates'),
        h('button', { onclick: () => { model.known = ''; root.querySelectorAll('input[type="datetime-local"]')[1].value = ''; return load(); } }, 'Current belief about selected world time')),
        h('p', { class: 'muted' }, 'Set both dates for what was believed then. Clear Known at to see today’s belief about that past period. Dates use your browser timezone.')),
      h('details', null, h('summary', null, 'Advanced: exact matches & scope'),
        h('div', { class: 'row' }, field('Agent', model, 'agent'), field('Run', model, 'run'),
          field('Target memory ID', model, 'target'), field('Slot', model, 'slot')),
        h('div', { class: 'row' }, field('Subject', model, 'subject'), field('Predicate', model, 'predicate'),
          field('Conflicting object', model, 'object'),
          field('Candidate cap (server may reduce)', model, 'limit', { type: 'number', min: 1, max: 64 })),
        h('p', { class: 'muted' }, 'This is a bounded candidate view, not a full database export. Other versions may include assertions not yet known at the selected date.'))),
    error, output);
  await load();
}
