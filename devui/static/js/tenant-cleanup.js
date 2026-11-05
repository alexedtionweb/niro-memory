import { state, call, h, guard, toast } from './lib.js';

// Cleanup is tenant-wide, never scoped to the visible page or project filter.
export function tenantCleanup(active, afterComplete) {
  const tenant = state.info?.tenant || state.tenant;
  const selectedTenant = state.tenant;
  const name = h('input', { placeholder: tenant, 'aria-label': 'Type tenant name to delete everything', autocomplete: 'off' });
  const progress = h('p', { role: 'status' });
  let busy = false, requestID = '', complete = false;
  const valid = () => active() && state.tenant === selectedTenant;
  const show = async result => {
    if (!valid()) return;
    progress.textContent = result.state === 'complete'
      ? 'Tenant cleanup completed. All stored tenant data and originals were deleted.'
      : `Cleanup queued / running. ${result.deleted || 0} database rows deleted; table stage ${result.phase || 0}. Writes are locked until completion. Check Jobs for worker failures.`;
    if (result.state === 'complete' && !complete) {
      complete = true;
      requestID = '';
      state.info = null;
      await afterComplete();
    }
  };
  const remove = h('button', { class: 'danger', disabled: true, onclick: guard(async () => {
    if (!valid() || busy || name.value !== tenant) return;
    if (!window.confirm(`PERMANENTLY DELETE EVERYTHING in tenant "${tenant}"? This includes ALL projects, memories, conversations, files, vectors, jobs and tenant settings. This cannot be undone.`)) return;
    busy = true; remove.disabled = true; complete = false;
    requestID ||= crypto.randomUUID();
    progress.textContent = 'Submitting irreversible tenant cleanup...';
    try {
      const result = await call('POST', '/v1/maintenance/purge', { confirmation: name.value, request_id: requestID });
      if (!valid()) return;
      name.value = '';
      await show(result);
      toast('Cleanup accepted. Monitor progress or Jobs; closing this page does not cancel it.');
    } finally {
      busy = false;
      remove.disabled = !valid() || name.value !== tenant;
    }
  }) }, 'Delete all tenant data');
  name.addEventListener('input', () => { remove.disabled = busy || !valid() || name.value !== tenant; });
  return h('details', { class: 'card' }, h('summary', null, 'Danger zone: delete everything'),
    h('p', null, `This permanently deletes ALL data for tenant "${tenant}", across every project: memories and history, conversations, original files, vectors, relationships, jobs and tenant settings. Other tenants and server configuration are not affected. No undo.`),
    h('p', null, 'Deletion runs in bounded background batches. Stop external writers and wait for in-flight requests first. Writes are locked while cleanup runs; an active worker is required. Backups and S3 retained versions are not erased.'),
    h('label', null, `Type "${tenant}" to confirm`, name),
    h('div', { class: 'row' }, remove, h('button', { onclick: guard(async () => {
      if (!valid()) return;
      await show(await call('GET', '/v1/maintenance/purge'));
    }) }, 'Refresh cleanup status')), progress);
}
