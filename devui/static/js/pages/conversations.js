// Conversations: runs, their turns (with media previews), edits and
// tombstones, and the prompt window for a token budget.
import { call, h, page, table, short, badge, date, blobURL, guard, toast } from '../lib.js';

function part(p) {
  const out = [];
  if (p.text) out.push(h('div', null, p.text));
  if (p.caption) out.push(h('div', { class: 'muted' }, '🖼 ' + p.caption));
  if (p.derived_text) out.push(h('div', { class: 'muted' }, '⤷ ' + p.derived_text));
  if (p.kind === 'tool_call' || p.kind === 'tool_result' || p.kind === 'data') out.push(h('div', { class: 'mono' }, `${p.kind} ${p.name || ''} ${p.data ? JSON.stringify(p.data) : ''}`));
  if (p.blob) {
    const box = h('div', { class: 'mono muted' }, `${p.kind} ${p.mime} ${p.blob.size} bytes ${short(p.blob.sha256)}`);
    if ((p.mime || '').startsWith('image/')) blobURL(p.blob.sha256).then((u) => box.append(h('img', { src: u, alt: p.caption || 'image' }))).catch(() => {});
    out.push(box);
  }
  return out;
}

export default async function (root, [run, focus]) {
  const runs = await call('GET', '/_dev/api/runs');
  const left = h('div', { class: 'card' }, h('h2', null, 'Runs'),
    table([
      { label: 'run', render: (r) => h('b', null, r.run_id) }, { label: 'user', key: 'user_id' },
      { label: 'agent', key: 'agent_id' }, { label: 'turns', key: 'turns' }, { label: 'last', render: (r) => date(r.last) },
    ], runs, (r) => (location.hash = '#/conversations/' + encodeURIComponent(r.run_id))));
  const right = h('div');
  page(root, 'Conversations', 'The raw L0 log. Every memory points back to exact turns and byte spans here.', h('div', { class: 'grid2' }, left, right));
  if (!run) return;
  const { turns } = await call('GET', `/v1/runs/${encodeURIComponent(run)}/turns?include_deleted=true&last=200`);
  const budget = { tokens: 300 };
  const windowBox = h('div');
  const showWindow = guard(async () => {
    const w = await call('GET', `/v1/runs/${encodeURIComponent(run)}/window?tokens=${budget.tokens}`);
    windowBox.replaceChildren(h('p', { class: 'muted' }, `${w.turns.length} turns verbatim · ${w.tokens} tokens · ${w.omitted} omitted` + (w.summary ? ' · summary up to #' + w.summary_upto : '')),
      ...(w.summary ? [h('div', { class: 'turn' }, h('b', null, 'summary: '), w.summary)] : []),
      w.turns.map((t) => h('div', { class: 'turn ' + t.role }, h('b', null, t.role + ': '), t.parts.map((p) => p.text || p.caption).join(' '))));
  });
  const summary = h('textarea', { rows: 2, placeholder: 'Rolling summary of older turns (written by your app or a summarizer extension)' });
  const tokens = h('input', { type: 'range', min: 20, max: 4000, value: budget.tokens });
  tokens.addEventListener('input', () => { budget.tokens = Number(tokens.value); showWindow(); });
  const last = turns.length ? turns[turns.length - 1].seq : 0;
  right.replaceChildren(
    h('div', { class: 'card' }, h('h2', null, 'Run ' + run),
      turns.map((t) => h('div', { class: `turn ${t.role} ${t.status === 'deleted' ? 'deleted' : ''}`, id: 't-' + t.id },
        h('div', { class: 'muted mono' }, `#${t.seq} ${t.role}${t.actor ? ' (' + t.actor + ')' : ''} · ${date(t.occurred_at)} · ${short(t.id)} `,
          t.status !== 'final' ? badge(t.status, 'warn') : null, focus === t.id ? badge('evidence', 'ok') : null,
          t.status !== 'deleted' ? h('button', { onclick: guard(async () => { await call('DELETE', '/v1/turns/' + t.id); toast('Turn deleted; derived memories re-derived by the worker'); location.reload(); }) }, 'delete') : null),
        t.parts.map(part)))),
    h('div', { class: 'card' }, h('h2', null, 'Prompt window'), h('label', null, 'token budget ', tokens), windowBox,
      h('h2', null, 'Summary'), summary,
      h('div', { class: 'row' }, h('button', { onclick: guard(async () => { await call('PUT', `/v1/runs/${encodeURIComponent(run)}/summary`, { text: summary.value, upto: last }); toast('Summary saved'); showWindow(); }) }, `Save summary (covers #1–#${last})`))));
  showWindow();
  if (focus) document.getElementById('t-' + focus)?.scrollIntoView({ block: 'center' });
}
