import { state, call, h, pre, badge, date, ms } from './lib.js';

export function workspaceSession(root) {
  const tenant = state.tenant, key = state.key;
  return () => root.isConnected && state.tenant === tenant && state.key === key;
}

export function statePath(request) {
  const input = request.input || {}, scope = input.scope || {};
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries({
    user_id: scope.user_id, agent_id: scope.agent_id, run_id: scope.run_id,
    target_id: input.target_id, text: input.text, slot: input.slot,
    subject: input.assertion?.subject, predicate: input.assertion?.predicate, object: input.assertion?.object,
    valid_at: request.valid_at, known_at: request.known_at, max_candidates: request.max_candidates,
  })) if (v !== undefined && v !== '') q.set(k, v);
  if (request.all_versions) q.set('all_versions', 'true');
  return '/v1/knowledge/state?' + q;
}

export function memoryLink(id, label) {
  return id ? h('a', { href: '#/memory/' + encodeURIComponent(id) }, label || id) : '';
}

export function renderEnrichment(result) {
  const labels = { queued: 'Waiting for extraction', processing: 'Extracting knowledge',
    completed: 'Extraction complete', failed: 'Extraction failed', shadow: 'Test extraction — not applied',
    partial: 'Partial extraction — rejected records need review',
    empty: 'No structured records extracted — source preserved',
    disabled: 'Extraction paused', not_scheduled: 'No extraction job recorded', needs_review: 'Extraction needs review',
    source_only: 'Source passages indexed — no duplicate extraction' };
  return h('section', { class: 'card', 'aria-live': 'polite' },
    h('h3', null, labels[result.status] || result.status),
    h('p', { class: 'muted' }, 'The original is saved separately. Extracted interpretations are unreviewed, not automatic corrections.'),
    result.error ? h('p', { class: 'error-message', role: 'alert' }, result.error) : null,
    result.reason ? h('p', { class: 'muted' }, result.reason) : null,
    result.extraction?.issues?.length ? h('details', { open: true }, h('summary', null, 'Rejected records / extraction gaps'),
      result.extraction.issues.map(issue => h('p', { class: 'error-message' }, `${issue.stage} ${issue.index}: ${issue.reason}`))) : null,
    result.extraction ? h('p', null, `${result.extraction.model || result.extraction.provider || 'Provider not reported'} · ${ms(result.duration_ns || 0)} · ${(result.extraction.facts || []).length} extracted claims · ${(result.extraction.excerpts || []).length} source excerpts`) : null,
    result.extraction?.normalization_notes?.length ? h('details', null, h('summary', null, 'Extraction validation notes'),
      result.extraction.normalization_notes.map(note => h('p', { class: 'muted' }, note))) : null,
    (result.extraction?.facts || []).map(f => h('article', { class: 'suggestion' },
      h('p', null, `${f.subject} · ${f.negated ? 'NOT ' : ''}${f.predicate} · ${f.object}`),
      badge(f.type), f.model_type ? h('p', { class: 'muted' }, `Classified from explicit decision predicate; model originally labeled it ${f.model_type}.`) : null,
      h('blockquote', null, f.quote))),
    (result.extraction?.excerpts || []).map(text => h('article', { class: 'suggestion' },
      h('p', { class: 'muted' }, 'Verbatim source excerpt — no structured assertion'),
      h('blockquote', null, text))),
    result.extraction?.entities?.length ? h('div', null, h('h4', null, 'Named entities'),
      result.extraction.entities.map(e => h('article', { class: 'suggestion' },
        h('p', null, e.name, ' ', badge(e.type)),
        Object.keys(e.attributes || {}).length ? pre(e.attributes) : null,
        h('blockquote', null, e.quote)))) : null,
    (result.changes || []).map(c => h('p', null, c.action === 'evidence'
      ? 'Existing memory matched — no duplicate created. ' : 'Extracted memory saved. ',
      memoryLink(c.memory.id, c.action === 'evidence' ? 'Read reinforced memory' : 'Read extracted memory'))),
    (result.relations || []).map(r => h('p', null, memoryLink(r.source_id), ` ${r.relation.replaceAll('_', ' ')} `, memoryLink(r.target_id),
      r.relation === 'contradicts' ? ' — potential conflict, needs review' : '')),
    h('details', null, h('summary', null, 'Extraction evidence & diagnostics'), pre(result)));
}

export async function inspectEnrichment(root, id) {
  const active = workspaceSession(root);
  const view = h('div', null, h('p', { role: 'status' }, 'Checking background extraction status…'));
  let generation = 0;
  const refresh = async () => {
    const current = ++generation;
    try {
      const result = await call('GET', '/v1/knowledge/enrichment/' + encodeURIComponent(id));
      if (!active() || current !== generation) return;
      view.replaceChildren(renderEnrichment(result));
      if (['queued', 'processing'].includes(result.status)) {
        const timer = setTimeout(() => { if (active() && current === generation) void refresh(); }, 1500);
        timer.unref?.();
      }
    } catch (e) {
      if (active() && current === generation) view.replaceChildren(h('p', { role: 'alert' }, 'Extraction status unavailable: ' + e.message));
    }
  };
  root.replaceChildren(view, h('button', { onclick: refresh }, 'Refresh extraction status'));
  await refresh();
}

export function renderSystems(info, coverage, coverageError) {
  const c = info.capabilities || {}, knowledge = info.knowledge?.effective;
  const mode = knowledge?.mode || c.knowledge_mode;
  const embedder = c.embedder, semantic = embedder && embedder !== 'hash@1';
  return h('div', { class: 'grid2' },
    h('section', { class: 'system-status' }, h('h3', null, knowledge?.background_enrichment ? 'Extraction & decision model' : 'Decision planner'),
      h('strong', null, knowledge?.model || knowledge?.provider || 'Not reported'),
      h('p', null, mode === 'enforced' ? knowledge?.background_enrichment ? 'Live — background extraction enabled.' : 'Live — confirmed saves are enabled.'
        : mode === 'shadow' ? 'Test only — suggestions do not change memories.'
          : mode === 'disabled' ? 'Paused — not processing suggestions.' : 'Mode not reported.'),
      h('p', { class: 'muted' }, knowledge?.background_enrichment
        ? 'Save first, extract claims and connections in the background. Configuration is not a model health check.'
        : 'Proposes memory actions; deterministic safety checks decide what is allowed. Configuration is not a model health check.')),
    h('section', { class: 'system-status' }, h('h3', null, 'Embedding system'),
      h('strong', null, embedder || 'Not configured'),
      h('p', null, semantic ? 'Semantic text embeddings configured.'
        : embedder ? 'Test hash vectors only — not semantic.' : 'Semantic embeddings are off. Text search still works.'),
      coverageError ? h('p', { class: 'error-message', role: 'alert' }, 'Embedding coverage unavailable: ' + coverageError)
        : coverage ? h('p', null, `${coverage.have} / ${coverage.total} memories embedded`,
          coverage.total === 0 ? ' — no memories to embed yet.'
            : coverage.have < coverage.total ? ' — some still need background processing.' : ' — coverage complete.')
          : semantic ? h('p', null, 'Coverage not checked yet.') : null,
      h('a', { href: '#/embeddings' }, 'Embedding details & backfill')));
}

export function memoryCard(m) {
  const assertion = m.metadata?.assertion, decision = m.metadata?.decision;
  return h('article', { class: 'card knowledge-memory' },
    h('p', { class: 'memory-content' }, m.text || m.title || '(No text recorded)'),
    h('div', { class: 'row muted' }, badge(`Version ${m.version}`),
      h('span', null, m.user_id || 'No project assigned'),
      h('span', null, m.recorded_at ? `Learned ${date(m.recorded_at)}` : 'Recorded time unavailable')),
    assertion ? h('p', null, `${assertion.subject} · ${assertion.conditions?.polarity === 'negative' ? 'NOT ' : ''}${assertion.predicate} · ${assertion.object}`) : null,
    m.metadata?.enrichment ? h('p', { class: 'muted' }, 'Unreviewed extraction · ', memoryLink(m.metadata.enrichment.source_id, 'Original source')) : null,
    (m.entities || []).length ? h('p', null, 'Entities: ', m.entities.map(e => badge(e.type ? `${e.name} (${e.type})` : e.name))) : null,
    m.metadata?.extracted_entities?.length ? h('details', null, h('summary', null, 'Original entity annotations (unreviewed)'), pre(m.metadata.extracted_entities)) : null,
    decision ? h('p', null, `Decision: ${decision.status || 'status not recorded'}${decision.outcome ? ' — ' + decision.outcome : ''}`) : null,
    h('div', { class: 'row' }, memoryLink(m.id, 'Read memory & history'),
      h('a', { href: '#/intelligence/' + encodeURIComponent(m.id) }, 'Suggest a correction')),
    h('details', null, h('summary', null, 'Evidence & technical details'), pre(m)));
}

export function renderState(value) {
  const categories = [
    ['current', 'What is known'], ['historical', 'Other versions'],
    ['conflicts', 'Conflicting memories'], ['decisions', 'Decisions'],
    ['supporting_evidence', 'Supporting evidence'],
  ];
  return h('div', { class: 'knowledge-panel' },
    h('p', { class: 'muted' }, `${(value.current || []).length} matching memories in this bounded view. Open a memory for its full history.`),
    categories.map(([key, title]) => {
    const records = value[key] || [];
    return h('details', { open: key === 'current' || key === 'conflicts' && records.length > 0 },
      h('summary', null, `${title} (${records.length})`),
      records.length ? records.map(memoryCard) : h('p', { class: 'muted' }, key === 'current'
        ? 'No matching memories returned. Try another project, search or date.' : 'None returned for this query.'));
  }), ['relations', 'unresolved_relationships'].map(key =>
    h('details', null, h('summary', null, `${key === 'relations' ? 'Connections' : 'Connections needing review'} (${(value[key] || []).length})`),
      (value[key] || []).length ? (value[key] || []).map(e => h('article', { class: 'card' },
        h('p', null, memoryLink(e.source_id), ` ${e.relation.replaceAll('_', ' ')} `, memoryLink(e.target_id)),
        h('p', { class: 'muted' }, e.reason || 'No explanation recorded.'),
        h('details', null, h('summary', null, 'Connection details'), pre(e)),
          e.origin === 'validated' && e.operation_key
            ? h('a', { href: '#/transitions/' + encodeURIComponent(e.operation_key) }, 'Inspect relationship transition') : null))
        : h('p', { class: 'muted' }, 'None returned.'))),
  h('details', null, h('summary', null, 'Advanced: raw state response'), pre(value)));
}

const operationLabels = {
  ADD: 'Remember a new fact', REINFORCE: 'Add support to an existing memory',
  REFINE: 'Add a more precise memory', SUPERSEDE: 'Replace a memory because things changed',
  CORRECT: 'Correct an earlier memory', RETRACT: 'Withdraw a memory',
  BRANCH: 'Keep an alternative for a different situation', RELATE: 'Connect existing memories',
  DEFER: 'Wait for more information', IGNORE: 'Make no change',
};

export function transitionOutcome(result) {
  const count = (result.changes || []).length;
  if (result.status === 'proposed') return { title: 'Preview only — nothing saved', note: 'Review the suggestion below. No memory or result has been stored yet.', kind: 'warn' };
  if (result.status === 'shadow') return { title: 'Test result recorded — memories unchanged', note: 'You are in test mode (shadow). The suggestion was recorded for inspection, not applied to your memories.', kind: 'warn' };
  if (result.status === 'failed') return { title: 'This attempt failed', note: 'The planner or validator rejected this attempt. Review the error below.', kind: 'bad' };
  if (result.status === 'committed') return { title: count ? `${count} memory change${count === 1 ? '' : 's'} saved` : 'Result recorded — no memory versions changed',
    note: count ? 'The validated changes were applied. Open a memory below to read its history.'
      : 'The operation completed. It may have changed connections only or deliberately made no change; inspect the actions below.', kind: 'ok' };
  return { title: 'Recorded result', note: `Backend status: ${result.status || 'not recorded'}. No save outcome has been inferred.`, kind: '' };
}

export function renderTransition(result) {
  const outcome = transitionOutcome(result);
  return h('div', { class: 'knowledge-panel' },
    h('div', { class: 'outcome outcome-' + outcome.kind, role: 'status' },
      h('h3', null, outcome.title), h('p', null, outcome.note),
      result.replayed ? h('p', null, 'This is the same saved result from your earlier attempt, not a duplicate write.') : null),
    result.error ? h('p', { class: 'error-message', role: 'alert' }, result.error) : null,
    h('p', { class: 'muted' }, `${result.plan?.model || result.plan?.provider || 'Planner not recorded'} · ${result.validation === 'passed'
      ? 'Safety checks passed' : result.validation === 'failed' ? 'Safety checks failed' : 'Safety check result not recorded'}`),
    (result.plan?.operations || []).map(o => h('article', { class: 'suggestion' },
      h('h3', null, operationLabels[o.type] || o.type),
      o.memory?.text ? h('p', { class: 'memory-content' }, o.memory.text) : null,
      o.target_id ? h('p', null, 'Existing memory: ', memoryLink(o.target_id, 'Read the original')) : null,
      h('p', { class: 'muted' }, 'Reason: ', (o.reason_code || 'not recorded').replaceAll('_', ' ')))),
    result.changes?.length ? h('div', null, h('h3', null, 'Changed memories'),
      result.changes.map(c => h('p', null, memoryLink(c.memory?.id, c.memory?.text || c.action)))) : null,
    h('details', { class: 'card' }, h('summary', null, 'Advanced: how this result was produced'),
      h('p', null, `Status: ${result.status} · Mode: ${result.mode} · ${result.replayed ? 'idempotent replay' : 'original result'}`),
      h('p', null, result.duration_ns === undefined ? 'duration not recorded' : 'Planning + validation: ' + ms(result.duration_ns)),
      result.validation_error ? pre(result.validation_error) : null,
      h('h3', null, 'Validated operations and reason codes'), pre(result.plan),
      h('h3', null, 'Provider proposal before validation'),
      result.proposed_plan ? pre(result.proposed_plan) : h('p', null, 'Not recorded in this trace. No proposal has been reconstructed.'),
      h('h3', null, 'Persisted memory changes'), pre(result.changes || []),
      h('h3', null, 'Complete transition receipt, binding hashes and policy version'), pre(result)));
}

export async function scopedInput(id) {
  if (!id) return { scope: { user_id: '', agent_id: '' } };
  const m = await call('GET', '/v1/memories/' + encodeURIComponent(id));
  return { scope: { user_id: m.user_id || '', agent_id: m.agent_id || '', run_id: m.run_id || '' },
    target_id: m.id, slot: m.slot || '', text: m.text, visibility: m.visibility || 'team', grants: m.grants };
}
