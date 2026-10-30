// Add playground: send turns (text and media) and watch what the pipeline
// does: rule matches, validation, reconciliation (add / supersede /
// duplicate / invalidate / reject).
import { call, h, page, table, badge, short, guard, field, choose, toast } from '../lib.js';

const t = { user_id: 'alice', agent_id: 'assistant', run_id: 'playground', role: 'user', text: '', wait: true, fact: '', slot: '' };

const actionKind = { add: 'ok', supersede: 'warn', invalidate: 'bad', reject: 'bad', evidence: '', noop: '' };

function changes(list) {
  return table([
    { label: 'action', render: (c) => badge(c.action, actionKind[c.action]) },
    { label: 'memory', render: (c) => [c.memory.text || h('i', { class: 'muted' }, '(none)'), c.memory.slot ? h('div', { class: 'mono muted' }, c.memory.slot) : null] },
    { label: 'previous', render: (c) => (c.previous ? h('a', { href: '#/memory/' + c.previous }, short(c.previous)) : '') },
    { label: 'reason', key: 'reason' },
    { label: 'source', render: (c) => (c.memory.source ? h('span', { class: 'mono muted' }, c.memory.source.derived_by || c.memory.source.kind) : '') },
  ], list || [], (c) => c.memory.id && (location.hash = '#/memory/' + c.memory.id));
}

export default async function (root) {
  const file = h('input', { type: 'file', accept: 'image/*,audio/*' });
  const caption = h('input', { placeholder: 'caption / transcript of the attachment' });
  const out = h('div');
  const send = guard(async () => {
    const parts = [];
    if (t.text) parts.push({ kind: 'text', text: t.text });
    if (file.files[0]) {
      const f = file.files[0];
      const b64 = await new Promise((ok) => { const r = new FileReader(); r.onload = () => ok(r.result.split(',')[1]); r.readAsDataURL(f); });
      parts.push({ mime: f.type, name: f.name, caption: caption.value, bytes: b64 });
    }
    if (!parts.length) return toast('Write something or attach a file', true);
    const preview = t.text ? await call('POST', '/_dev/api/rules/test', { text: t.text, role: t.role, user_id: t.user_id }).catch(() => null) : null;
    const r = await call('POST', `/v1/runs/${encodeURIComponent(t.run_id)}/turns`, {
      user_id: t.user_id, agent_id: t.agent_id, wait: t.wait, turns: [{ role: t.role, parts }] });
    out.prepend(h('div', { class: 'card' },
      h('h2', null, `Turn #${r.episodes[0].seq} ${r.duplicates[0] ? '(duplicate)' : ''} → `, h('a', { href: '#/conversations/' + encodeURIComponent(t.run_id) + '/' + r.episodes[0].id }, 'view in conversation')),
      preview && !preview.error ? h('p', { class: 'muted' }, `rules ${preview.version}: ${preview.candidates.length} candidates`, (preview.flags || []).map((f) => [' ', badge(f, 'bad')])) : null,
      t.wait ? changes(r.changes) : h('p', { class: 'muted' }, 'Queued: a worker will process this run after the debounce (see Jobs).')));
  });
  const remember = guard(async () => {
    const r = await call('POST', '/v1/facts', { facts: [{ user_id: t.user_id, agent_id: t.agent_id, text: t.fact, slot: t.slot || undefined }] });
    out.prepend(h('div', { class: 'card' }, h('h2', null, 'Explicit fact'), changes(r.changes)));
  });
  page(root, 'Add playground', 'Two ways to remember: append conversation turns (rules extract facts) or write facts explicitly. Try the same slot twice to see supersession.',
    h('div', { class: 'grid2' },
      h('div', { class: 'card' }, h('h2', null, 'Append a turn'),
        h('div', { class: 'row' }, field('user', t, 'user_id'), field('agent', t, 'agent_id'), field('run', t, 'run_id'), choose('role', t, 'role', ['user', 'assistant', 'tool', 'event'])),
        field('text', t, 'text', { textarea: true, rows: 3, placeholder: 'My name is Alice and I live in Lisbon.' }),
        h('div', { class: 'row' }, h('label', null, 'attachment', file), h('label', null, 'caption', caption)),
        h('div', { class: 'row' }, field('process now (wait)', t, 'wait', { type: 'checkbox' }), h('button', { class: 'primary', onclick: send }, 'Append'))),
      h('div', { class: 'card' }, h('h2', null, 'Remember a fact'),
        field('text', t, 'fact', { textarea: true, rows: 3, placeholder: 'Alice prefers aisle seats' }),
        h('div', { class: 'row' }, field('slot (optional)', t, 'slot', { placeholder: 'user.seat' }), h('button', { class: 'primary', onclick: remember }, 'Remember')))),
    out);
}
