// Jobs: the durable queue — pending, leased and dead jobs, retry dead ones,
// and run a worker pass on demand.
import { call, h, page, table, badge, short, date, guard, toast, choose } from '../lib.js';

const q = { state: '', all: false };

export default async function (root) {
  const out = h('div');
  const load = guard(async () => {
    const jobs = await call('GET', `/_dev/api/jobs?state=${q.state}${q.all ? '&all=1' : ''}`);
    out.replaceChildren(table([
      { label: 'id', render: (j) => h('span', { class: 'mono' }, short(j.id)) },
      { label: 'tenant', key: 'tenant' }, { label: 'kind', render: (j) => h('b', null, j.kind) }, { label: 'key', key: 'key' },
      { label: 'state', render: (j) => badge(j.state, j.state === 'dead' ? 'bad' : j.state === 'leased' ? 'warn' : '') },
      { label: 'attempts', key: 'attempts' },
      { label: 'due / lease', render: (j) => (j.state === 'leased' ? `${j.worker} until ${date(j.lease_until)}` : date(j.not_before)) },
      { label: 'error', render: (j) => h('span', { class: 'mono' }, j.last_error || '') },
      { label: '', render: (j) => (j.state === 'dead' ? h('button', { onclick: guard(async () => { await call('POST', '/_dev/api/jobs/retry', { id: j.id }); toast('Requeued'); load(); }) }, 'retry') : null) },
    ], jobs || []));
  });
  const all = h('label', { class: 'check' }, h('input', { type: 'checkbox', onchange: (e) => { q.all = e.target.checked; load(); } }), 'all tenants');
  const st = choose('state', q, 'state', ['', 'pending', 'leased', 'dead']);
  st.querySelector('select').addEventListener('change', load);
  page(root, 'Jobs', 'Work is committed with the data that caused it (outbox). Leases make workers safe to run anywhere; crashed workers\' jobs are reclaimed.',
    h('div', { class: 'card' }, h('div', { class: 'row' }, st, all,
      h('button', { onclick: load }, 'Refresh'),
      h('button', { class: 'primary', onclick: guard(async () => { const r = await call('POST', '/_dev/api/jobs/run'); toast(`Processed ${r.processed} jobs`); load(); }) }, 'Run worker once'))),
    out);
  load();
}
