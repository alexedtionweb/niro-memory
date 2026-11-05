import { state, call, h, page, table, badge, short, date, guard, toast, choose, pre } from '../lib.js';
import { workspaceSession } from '../knowledge-ui.js';
import { filePreview } from '../file-preview.js';

const q = { state: '', all: false };

export default async function (root) {
  const active = workspaceSession(root);
  const tenant = state.info?.tenant || state.tenant;
  const previews = new Map(), expanded = new Set();
  const out = h('div'), notice = h('p', { role: 'status' });
  let cursor = '', history = [], generation = 0, busy = false;
  const previous = h('button', { disabled: true, onclick: async () => { cursor = history.pop() || ''; await load(); } }, 'Previous jobs');
  const next = h('button', { disabled: true }, 'Next jobs');
  const reset = async () => { cursor = ''; history = []; await load(); };
  const load = guard(async () => {
    if (!active()) return;
    const token = ++generation;
    const response = await call('GET', `/_dev/api/jobs?state=${encodeURIComponent(q.state)}&after_id=${encodeURIComponent(cursor)}${q.all ? '&all=1' : ''}`);
    if (!active() || token !== generation) return;
    const rows = response || [], more = rows.length > 100, jobs = rows.slice(0, 100);
    previous.disabled = !history.length || busy;
    next.disabled = !more || busy;
    nextCursor = more ? jobs.at(-1).id : '';
    notice.textContent = `${jobs.length} jobs in this queue page${more ? '; more available' : ''}. File stages are grouped below. Updated ${new Date().toLocaleTimeString()}.`;
    const action = (j, label, value, extra = {}) => h('button', {
      disabled: busy || j.tenant !== tenant,
      onclick: async () => {
        if (!active() || busy || j.tenant !== tenant) return;
        if (['cancel', 'delete'].includes(value) && !window.confirm(`${label} job ${j.id}? ${value === 'cancel' ? 'Cancellation is cooperative; committed changes remain.' : 'This removes the job record, not its stored memories.'}`)) return;
        busy = true;
        try {
          await call('POST', '/_dev/api/jobs/control', { id: j.id, action: value, ...extra });
          if (active()) toast(`${label}: accepted`);
        } finally {
          busy = false;
          if (active()) await load();
        }
      },
    }, label);
    const columns = [
      { label: 'id / tenant', render: j => h('span', null, h('span', { class: 'mono', title: j.id }, short(j.id)), h('small', null, ' / ', j.tenant)) },
      { label: 'kind / key', render: j => h('span', null, h('b', null, j.kind), h('div', { class: 'mono' }, j.key)) },
      { label: 'state', render: j => badge(j.state, j.state === 'dead' ? 'bad' : j.state === 'leased' ? 'warn' : '') },
      { label: 'priority', render: j => {
        const priority = h('input', { type: 'number', min: -100, max: 100, value: j.priority || 0, 'aria-label': `Priority for ${j.id}`, disabled: busy || j.state === 'leased' || j.tenant !== tenant });
        const save = h('button', { disabled: busy || j.state === 'leased' || j.tenant !== tenant, onclick: async () => {
          const value = Number(priority.value);
          if (!Number.isInteger(value) || value < -100 || value > 100) throw new Error('Priority must be an integer from -100 to 100.');
          if (!active() || busy || j.tenant !== tenant) return;
          busy = true; save.disabled = true;
          try { await call('POST', '/_dev/api/jobs/control', { id: j.id, action: 'priority', priority: value }); }
          finally { busy = false; if (active()) await load(); }
        } }, 'Set priority');
        return h('div', { class: 'job-priority' }, priority, save);
      } },
      { label: 'attempts / timing', render: j => h('span', null, `${j.attempts} attempts`, h('div', null, j.state === 'leased'
        ? `${j.worker} · lease until ${date(j.lease_until)}`
        : j.state === 'pending' ? `Due ${date(j.not_before)}` : 'Not scheduled'), h('small', null, `Last update ${date(j.updated_at)}`)) },
      { label: 'last error / details', render: j => h('span', null, h('span', { class: 'mono' }, j.last_error || 'No recorded error'),
        h('details', null, h('summary', null, 'Payload / checkpoint'), pre({ payload: j.payload, checkpoint: j.checkpoint }))) },
      { label: 'actions', render: j => {
        const controls = [], protectedPurge = j.kind === 'tenant.purge';
        if (['pending', 'leased'].includes(j.state) && !protectedPurge) controls.push(action(j, 'Pause', 'pause'));
        if (j.state === 'paused') controls.push(action(j, 'Resume', 'resume'));
        if (['dead', 'cancelled'].includes(j.state)) controls.push(action(j, 'Retry', 'retry'));
        if (['pending', 'leased', 'paused'].includes(j.state) && !protectedPurge) controls.push(action(j, 'Cancel', 'cancel'));
        if (j.state !== 'leased' && !protectedPurge) controls.push(action(j, 'Delete job', 'delete'));
        return h('div', { class: 'row job-actions' }, ...controls,
          j.tenant !== tenant ? h('small', null, 'Switch to this tenant to manage it.') : null);
      } },
    ];
    const groups = new Map(), shared = [];
    for (const job of jobs) {
      if (!job.file) { shared.push(job); continue; }
      const key = JSON.stringify([job.tenant, job.file.id]);
      if (!groups.has(key)) groups.set(key, { file: job.file, tenant: job.tenant, jobs: [] });
      groups.get(key).jobs.push(job);
    }
    const cards = [];
    for (const [key, group] of groups) {
      const counts = new Map();
      for (const job of group.jobs) counts.set(job.state, (counts.get(job.state) || 0) + 1);
      const stages = h('details', { open: expanded.has(key) }, h('summary', null, `Processing stages (${group.jobs.length} on this page)`),
        table(columns, group.jobs));
      stages.addEventListener('toggle', () => { if (stages.open) expanded.add(key); else expanded.delete(key); });
      let preview = previews.get(key);
      if (!preview && group.tenant === tenant) {
        preview = filePreview(group.file, active);
        previews.set(key, preview);
      }
      cards.push(h('article', { class: 'card file-job' },
        h('h2', null, group.file.name), h('p', { class: 'muted' }, `Tenant ${group.tenant} · ${group.file.id}`),
        group.tenant === tenant ? h('a',{href:'#/documents/'+encodeURIComponent(group.file.id)},'Open document lineage and revisions') : null,
        h('p', null, ...[...counts].map(([status, count]) => badge(`${count} ${status}`, status === 'dead' ? 'bad' : status === 'leased' ? 'warn' : '')),
          'Stage counts reflect this queue page/filter, not completed work.'),
        preview || h('p', null, 'Switch to this tenant to preview its file.'), stages));
    }
    for (const key of previews.keys()) if (!groups.has(key)) previews.delete(key);
    if (shared.length) cards.push(h('section', { class: 'card' }, h('h2', null, 'Other / shared jobs'),
      h('p', { class: 'muted' }, 'Model-wide embedding and indexing jobs may process several files and are not assigned to a single file.'), table(columns, shared)));
    if (!jobs.length) cards.push(h('p', null, 'No outstanding jobs match this filter. Completed jobs are removed from the queue.'));
    out.replaceChildren(...cards);
  });
  let nextCursor = '';
  next.addEventListener('click', async () => { if (!nextCursor || busy) return; history.push(cursor); cursor = nextCursor; await load(); });
  const all = h('label', { class: 'check' }, h('input', { type: 'checkbox', checked: q.all, onchange: async e => { q.all = e.target.checked; await reset(); } }), 'all tenants');
  const st = choose('state', q, 'state', ['', 'pending', 'leased', 'paused', 'cancelled', 'dead']);
  st.querySelector('select').addEventListener('change', reset);
  page(root, 'Jobs', 'Durable background work. Higher priority runs first within each tenant; tenants still share worker capacity fairly.',
    h('div', { class: 'card' }, h('div', { class: 'row' }, st, all,
      h('button', { onclick: load }, 'Refresh'),
      h('button', { class: 'primary', disabled: state.info?.inline_worker, onclick: async () => {
        if (busy) return;
        busy = true;
        try { const r = await call('POST', '/_dev/api/jobs/run'); if (active()) toast(`Processed ${r.processed} jobs`); }
        finally { busy = false; if (active()) await load(); }
      } }, state.info?.inline_worker ? 'Worker running automatically' : 'Run worker once')),
    h('p', null, 'Pending waits for its due time and a worker. Leased is running; inspect lease and last update. Dead requires Retry. Paused and cancelled never run automatically. Pause/cancel interrupts running work on its next heartbeat; it does not undo committed changes.'),
    h('p', { class: 'muted' }, 'Refreshes every five seconds. Priority changes do not preempt running jobs. An external worker must use this server configuration.')),
    notice, out, h('div', { class: 'row' }, previous, next));
  await load();
  const refresh = () => {
    const timer = setTimeout(async () => {
      if (!active()) return;
      if (!busy) await load();
      refresh();
    }, 5000);
    timer.unref?.();
  };
  refresh();
}
