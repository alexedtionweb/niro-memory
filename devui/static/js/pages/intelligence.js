import { state, call, h, page, pre, badge } from '../lib.js';
import { statePath, scopedInput, renderState, renderTransition, renderSystems, workspaceSession, inspectEnrichment } from '../knowledge-ui.js';
import { documentImport } from '../document-import.js';

export default async function(root, [id], { settingsOnly = false } = {}) {
  root.setAttribute('class', 'knowledge-workspace');
  const active = workspaceSession(root);
  const [config, seed] = await Promise.all([
    call('GET', '/_dev/api/knowledge/config'), scopedInput(id),
  ]);
  if (!active()) return;
  const initial = { scope: seed.scope, text: seed.text || '', slot: seed.slot || '',
    target_id: seed.target_id || '', visibility: seed.visibility || 'team', grants: seed.grants,
    trust: 'high', intent: seed.target_id ? 'CORRECT' : 'ADD' };
  const editor = h('textarea', { rows: 18, 'aria-label': 'Knowledge input JSON', spellcheck: 'false' });
  editor.value = JSON.stringify(initial, null, 2);
  const text = h('textarea', { rows: 4, 'aria-label': 'What should I remember?', placeholder: 'For example: Our API uses PostgreSQL in production.', value: initial.text });
  const project = h('input', { value: seed.scope.user_id || '', 'aria-label': 'Project or user', placeholder: 'Optional, e.g. my-project' });
  const useJSON = h('input', { type: 'checkbox', 'aria-label': 'Use JSON instead of the form' });
  const step = h('p', { class: 'flow-progress', role: 'status' });
  const key = h('input', { 'aria-label': 'Stable operation key', value: crypto.randomUUID(), maxlength: 200, class: 'grow' });
  const error = h('p', { role: 'alert', class: 'error-message' });
  const resultView = h('div', { 'aria-live': 'polite' }), beforeView = h('div'), afterView = h('div');
  const confirmation = h('input', { type: 'checkbox', 'aria-label': 'Confirm this action' });
  const modeNotice = h('p');
  const systems = h('div', { 'aria-live': 'polite' });
  let systemGeneration = 0;
  async function refreshSystems() {
    const generation = ++systemGeneration;
    try {
      const info = await call('GET', '/_dev/api/info');
      if (!active() || generation !== systemGeneration) return;
      state.info = info;
      let coverage, coverageError;
      if (info.capabilities?.embedder) {
        try {
          coverage = await call('GET', '/v1/vectors/coverage?model=' + encodeURIComponent(info.capabilities.embedder));
        } catch (e) { coverageError = e.message; }
      }
      if (active() && generation === systemGeneration) systems.replaceChildren(renderSystems(info, coverage, coverageError));
    } catch (e) {
      if (active() && generation === systemGeneration)
        systems.replaceChildren(h('p', { class: 'error-message', role: 'alert' }, 'Model status unavailable: ' + e.message));
    }
  }
  let currentConfig = config, previewPayload = '', commitPayload = '', busy = false;
  const evaluate = h('button', { class: 'primary', onclick: preview, 'data-action': 'preview' }, 'See what happens');
  const commit = h('button', { class: 'primary', disabled: true, onclick: persist, 'data-action': 'commit' });
  const fresh = h('button', { onclick: newOperation, 'data-action': 'reset' }, 'Start over');
  let sourcePayload = '', sourceSaved = false;
  const quickSave = h('button', { class: 'primary', 'data-action': 'save-now', onclick: saveNow }, 'Save now');
  const fast = config.effective.background_enrichment && !id;
  const keepView = h('section', { class: 'card' }, h('h2', null, 'Keep this result?'),
    h('p', { class: 'muted' }, 'In test mode this records the suggestion only. In live mode it applies the validated changes. The backend checks the latest state again, so the final action may differ.'),
    h('div', { class: 'row' }, h('label', { class: 'check' }, confirmation, 'I reviewed the suggestion and confirm this action'), commit));
  const formInput = () => ({ ...initial, scope: { ...initial.scope, user_id: project.value.trim() }, text: text.value });
  function syncForm() {
    editor.value = JSON.stringify(formInput(), null, 2);
    invalidate();
  }
  function controls() {
    evaluate.disabled = busy || currentConfig.effective.mode === 'disabled' || !!commitPayload || !!sourcePayload;
    quickSave.disabled = busy || sourceSaved || !!commitPayload || useJSON.checked;
    quickSave.textContent = sourceSaved ? 'Saved' : sourcePayload ? 'Retry / retrieve saved memory' : busy ? 'Saving original…' : 'Save now';
    commit.disabled = busy || !previewPayload || !confirmation.checked || currentConfig.effective.mode === 'disabled';
    fresh.disabled = busy;
    text.disabled = project.disabled = !!commitPayload || !!sourcePayload || busy || useJSON.checked;
    useJSON.disabled = key.disabled = !!commitPayload || !!sourcePayload || busy;
    editor.disabled = !!commitPayload || !!sourcePayload || busy || !useJSON.checked;
    confirmation.disabled = busy;
    keepView.hidden = !previewPayload;
    const mode = currentConfig.effective.mode;
    modeNotice.replaceChildren(badge(mode === 'shadow' ? 'Test mode' : mode === 'enforced' ? 'Live mode' : 'Paused', mode === 'enforced' ? 'warn' : ''),
      ` ${currentConfig.effective.model || currentConfig.effective.provider} · `,
      mode === 'disabled' ? 'Analysis is paused. Enable it in Advanced settings below. State and historical traces remain readable.'
        : mode === 'shadow' ? 'Try safely: suggestions can be recorded without changing memory state.'
          : 'Changes can be saved to memory, but only after your review and confirmation.');
    evaluate.textContent = busy ? 'Working…' : 'See what happens';
    commit.textContent = commitPayload ? 'Retry / retrieve this result' : mode === 'shadow' ? 'Record test result' : 'Save to memory';
    step.textContent = busy ? 'Working with the backend — this may take a few seconds.'
      : commitPayload ? '3 · Result — your exact input is locked so a retry cannot create a duplicate.'
        : sourceSaved ? 'Original saved. Extraction runs in the background. Choose Start over to remember another memory.'
        : sourcePayload ? 'Original save attempted — input locked for safe retries; see the result below.'
        : previewPayload ? '2 · Review the suggestion — nothing has been saved yet.'
          : fast ? 'Save immediately. Extraction runs in the background.' : '1 · Write a memory, then see what the model suggests.';
    if (fast) {
      modeNotice.replaceChildren(badge(mode === 'shadow' ? 'Test extraction' : mode === 'disabled' ? 'Extraction paused' : 'Live extraction'),
        ' Your original is saved immediately. ', mode === 'shadow' ? 'Suggestions are inspected but not applied.'
          : mode === 'disabled' ? 'No model processing while paused.' : 'Facts, entities and connections are added asynchronously as unreviewed interpretations.');
    }
  }
  function invalidate() {
    if (busy || commitPayload) return;
    previewPayload = ''; confirmation.checked = false;
    resultView.replaceChildren(); beforeView.replaceChildren(); afterView.replaceChildren(); error.textContent = '';
    controls();
  }
  function newOperation() {
    commitPayload = ''; sourcePayload = ''; sourceSaved = false; previewPayload = ''; key.value = crypto.randomUUID();
    confirmation.checked = false; error.textContent = '';
    resultView.replaceChildren(); beforeView.replaceChildren(); afterView.replaceChildren(); controls();
  }
  function payload() {
    const input = useJSON.checked ? JSON.parse(editor.value) : formInput();
    if (!input || typeof input !== 'object' || Array.isArray(input)) throw new Error('Input must be a JSON object.');
    if (!useJSON.checked && !input.text.trim()) throw new Error('Write something to remember first.');
    if (!key.value.trim() || new TextEncoder().encode(key.value.trim()).length > 200) throw new Error('Operation key must be 1–200 bytes.');
    return JSON.stringify({ input: { ...input, operation_key: key.value.trim() } });
  }
  async function preview() {
    if (!active() || busy) return;
    busy = true; previewPayload = ''; confirmation.checked = false; error.textContent = '';
    resultView.replaceChildren(); beforeView.replaceChildren(); afterView.replaceChildren(); controls();
    try {
      const body = payload(), request = JSON.parse(body);
      const [before, result] = await Promise.all([
        call('GET', statePath(request)), call('POST', '/v1/knowledge/evaluate', request),
      ]);
      if (!active()) return;
      previewPayload = body;
      beforeView.replaceChildren(h('h3', null, 'Before this attempt'), renderState(before));
      resultView.replaceChildren(h('h2', null, 'What the model suggests'), renderTransition(result));
    } catch (e) {
      if (active()) error.textContent = 'Could not finish the preview: ' + e.message + '. Nothing was saved. You can try again.';
    } finally { busy = false; if (active()) controls(); }
  }
  async function saveNow() {
    if (!active() || busy || useJSON.checked) return;
    try {
      if (!sourcePayload) {
        const request = JSON.parse(payload()).input;
        sourcePayload = JSON.stringify({ facts: [{ ...request.scope, text: request.text,
          visibility: request.visibility, grants: request.grants, operation_key: request.operation_key }] });
      }
      busy = true; error.textContent = ''; controls();
      const result = await call('POST', '/v1/facts', JSON.parse(sourcePayload));
      if (!active()) return;
      const change = result.changes?.[0];
      if (!change?.memory?.id || !['add', 'supersede', 'evidence'].includes(change.action))
        throw new Error(change?.reason || 'Backend did not confirm a saved memory.');
      const progress = h('div');
      sourceSaved = true;
      resultView.replaceChildren(h('h2', null, 'Original saved'), h('a', { href: '#/memory/' + encodeURIComponent(change.memory.id) }, 'Read original memory'), progress);
      void inspectEnrichment(progress, change.memory.id);
      void refreshSystems();
    } catch (e) {
      if (active()) error.textContent = 'Save not confirmed: ' + e.message + '. Retry with the same key; do not assume nothing was saved.';
    } finally { busy = false; if (active()) controls(); }
  }
  async function persist() {
    if (!active() || busy || !previewPayload || !confirmation.checked) return;
    if (!commitPayload && payload() !== previewPayload) { invalidate(); error.textContent = 'Input changed. Preview again before committing.'; return; }
    // Keep this exact payload and key even when the response is lost.
    commitPayload ||= previewPayload;
    busy = true; error.textContent = ''; afterView.replaceChildren(); controls();
    try {
      const request = JSON.parse(commitPayload);
      const result = await call('POST', '/v1/knowledge/commit', request);
      if (!active()) return;
      resultView.replaceChildren(h('h2', null, 'What happened'), renderTransition(result),
        h('a', { href: '#/transitions/' + encodeURIComponent(request.input.operation_key) }, 'Open this saved result'));
      try {
        const after = await call('GET', statePath(request));
        if (active()) afterView.replaceChildren(h('h3', null, 'After this attempt'), renderState(after));
      } catch (e) {
        if (active()) error.textContent = 'Commit succeeded, but state refresh failed: ' + e.message;
      }
      if (active()) await refreshSystems();
    } catch (e) {
      if (active()) error.textContent = 'Commit not confirmed: ' + e.message + '. Retry with the same operation key; do not assume no write occurred.';
    } finally { busy = false; if (active()) controls(); }
  }
  editor.addEventListener('input', invalidate); key.addEventListener('input', invalidate);
  text.addEventListener('input', syncForm); project.addEventListener('input', syncForm);
  useJSON.addEventListener('change', () => { if (!useJSON.checked) syncForm(); else invalidate(); controls(); });
  confirmation.addEventListener('change', controls);

  const mode = h('select', { 'aria-label': 'Tenant intelligence mode' },
    ['', 'disabled', 'shadow', 'enforced'].filter(m => config.runtime || !m ||
      ['disabled', 'shadow', 'enforced'].indexOf(m) <= ['disabled', 'shadow', 'enforced'].indexOf(config.server.mode))
      .map(m =>       h('option', { value: m, selected: m === (config.policy?.mode || '') },
        (fast ? { disabled: 'Extraction paused', shadow: 'Test extraction — keep originals only', enforced: 'Live mode — add unreviewed extractions' }
          : { disabled: 'Paused', shadow: 'Test mode — do not change memories', enforced: 'Live mode — allow confirmed changes' })[m] || 'Use server default')));
  const planner = h('select', { 'aria-label': 'Planner profile' },
    ['', ...Object.keys(config.profiles).sort()].map(name =>
      h('option', { value: name, selected: name === (config.policy?.planner || '') }, name || 'Inherit startup planner')));
  const cap = h('input', { type: 'number', min: 0, max: config.server.max_candidates,
    value: config.policy?.max_candidates || 0, 'aria-label': 'Maximum candidates' });
  const hops = h('input', { type: 'number', min: 0, max: config.server.max_graph_hops,
    value: config.policy?.max_graph_hops || 0, 'aria-label': 'Maximum graph hops' });
  const configStatus = h('p', { role: 'status' }), configView = h('div', null, pre(config));
  const profileSummary = h('p');
  function updateProfileSummary(value) {
    profileSummary.replaceChildren(badge(value.effective.model || value.effective.provider), badge(value.effective.mode),
      ...(value.effective.extraction?.entity_types || []).map(type => badge(type)));
  }
  updateProfileSummary(config);
  const inheritExtraction = h('input', { type: 'checkbox', checked: !config.policy?.extraction, 'aria-label': 'Use server extraction defaults' });
  const extraction = h('textarea', { rows: 16, 'aria-label': 'Extraction profile JSON', spellcheck: 'false',
    value: JSON.stringify(config.policy?.extraction || config.effective.extraction || {}, null, 2), disabled: inheritExtraction.checked });
  const schemaView = h('div', null, config.effective.extraction_schema ? pre(config.effective.extraction_schema) : 'Schema not reported by this server.');
  inheritExtraction.addEventListener('change', () => { extraction.disabled = inheritExtraction.checked; });
  const save = h('button', { onclick: async () => {
    if (!active() || busy) return;
    if (commitPayload || sourcePayload) { configStatus.textContent = 'Start a new operation before changing configuration. Keep the existing key for any unconfirmed commit retry.'; return; }
    busy = true; controls(); configStatus.textContent = '';
    try {
      if (!Number.isInteger(Number(cap.value)) || !Number.isInteger(Number(hops.value))) throw new Error('Limits must be integers.');
      const settings = await call('GET', '/v1/settings');
      if (!active()) return;
      settings.knowledge = { ...settings.knowledge, mode: mode.value, planner: planner.value,
        max_candidates: Number(cap.value), max_graph_hops: Number(hops.value) };
      if (inheritExtraction.checked) delete settings.knowledge.extraction;
      else settings.knowledge.extraction = JSON.parse(extraction.value);
      await call('PUT', '/v1/settings', settings);
      if (!active()) return;
      const value = await call('GET', '/_dev/api/knowledge/config');
      if (!active()) return;
      currentConfig = value; configView.replaceChildren(pre(value));
      updateProfileSummary(value);
      schemaView.replaceChildren(value.effective.extraction_schema ? pre(value.effective.extraction_schema) : 'Schema not reported by this server.');
      if (state.info) {
        state.info.capabilities.knowledge_mode = value.effective.mode;
        state.info.knowledge = value;
      }
      window.dispatchEvent(new Event('niromem:config'));
      await refreshSystems();
      if (!active()) return;
      configStatus.textContent = 'Saved for the authenticated tenant. Other nodes observe settings within their cache TTL.';
      commitPayload = ''; previewPayload = ''; confirmation.checked = false;
      resultView.replaceChildren(); beforeView.replaceChildren(); afterView.replaceChildren();
    } catch (e) {
      if (active()) configStatus.textContent = 'Configuration error: ' + e.message;
    } finally { busy = false; if (active()) controls(); }
  } }, 'Save tenant intelligence configuration');

  if (settingsOnly) {
    page(root, 'Extraction schema', 'The effective tenant profile and the exact JSON Schema sent to the model. Changes affect future extraction jobs; saved plans retain their original schema.',
      h('section', { class: 'card' }, h('h2', null, 'Active extraction profile'),
        profileSummary,
        h('div', { class: 'row' }, h('label', null, 'Mode', mode), h('label', null, 'Model profile', planner),
          h('label', null, 'Candidates', cap), h('label', null, 'Graph hops', hops))),
      h('section', { class: 'card' }, h('h2', null, 'Entity types & optional attributes'),
        h('label', { class: 'check' }, inheritExtraction, 'Use server extraction defaults'), extraction,
        save, configStatus),
      h('section', { class: 'card' }, h('h2', null, 'Effective JSON Schema'), schemaView),
      h('details', { class: 'card' }, h('summary', null, 'Configuration diagnostics'), configView));
    controls();
    return;
  }
  page(root, 'Try a memory', fast ? 'Save the original now. See extracted knowledge and connections appear below.' : 'Write something, review the suggestion, then decide whether to keep the result. Nothing is saved automatically.',
    h('div', { class: 'card mode-banner' }, modeNotice),
    h('div', { class: 'card' }, systems,
      h('button', { onclick: () => { if (active()) return refreshSystems(); } }, 'Refresh model status')),
    h('section', { class: 'card' }, step,
      h('h2', null, id ? 'What should this memory say instead?' : 'What should I remember?'),
      text, h('div', { class: 'row' }, h('label', null, 'Project or user (optional)', project)),
      h('p', { class: 'muted' }, id ? 'This starts a correction of the memory you selected. The original stays in its history.'
        : fast ? 'Use everyday language. Saving does not wait for a model. Extraction is bounded to four claims per source.'
          : 'Use everyday language. The model suggests an action; the backend checks whether it is allowed.'),
      h('div', { class: 'row' }, fast ? quickSave : evaluate, fresh),
      fast ? h('details', null, h('summary', null, 'Advanced: preview a transition instead'), evaluate) : null),
    error, resultView,
    !id ? documentImport(root, project) : null,
    !id ? documentImport(root, project, { media: true }) : null,
    keepView,
    h('details', { class: 'card' }, h('summary', null, 'Compare memories before & after'), beforeView, afterView,
      h('p', { class: 'muted' }, 'These are bounded, session-only snapshots. Other writers can change state between reads.')),
    h('details', { class: 'card' }, h('summary', null, 'Advanced: input, evidence & retry key'),
      h('label', { class: 'check' }, useJSON, 'Use JSON instead of the form'),
      h('p', { class: 'muted' }, 'Enable JSON to set scope, intent, target_id, visibility, grants, trust, assertion, decision, evidence or valid_from. Turning it off returns to the form values.'),
      editor, h('label', null, 'Stable operation key', key),
      h('p', { class: 'muted' }, 'The same input and key are kept after a write attempt, even if the response is lost. Retry to retrieve the result; do not start over to retry an unconfirmed write.')),
    h('details', { class: 'card' }, h('summary', null, 'Advanced: model & settings'),
      h('p', null, config.runtime
        ? 'Runtime mode and provider selection are enabled for this tenant, including activation from a disabled startup mode. Changes are durable and do not change other tenants.'
        : 'Startup permissions cannot be increased. Runtime provider selection is not registered on this server.'),
      h('p', { class: 'muted' }, 'Provider profiles are installed/configured by the server, not browser code. Credentials and endpoints are never returned here. Register additional knowledge.profiles in the CLI config to switch between Ollama and OpenAI-compatible models. 0 limits inherit server bounds.'),
      h('div', { class: 'row' }, h('label', null, 'Mode', mode), h('label', null, 'Provider profile', planner),
        h('label', null, 'Candidates', cap), h('label', null, 'Graph hops', hops), save), configStatus,
      h('details', null, h('summary', null, 'Extraction: entity types, attributes & JSON Schema'),
        h('p', null, 'Strong defaults extract named entities and grounded claims. Customize allowed entity types, fact types and optional entity_attributes (string, number or boolean; string enums supported). Safety and evidence fields cannot be removed.'),
        h('label', { class: 'check' }, inheritExtraction, 'Use server extraction defaults'),
        h('label', null, 'Controlled extraction profile (JSON)', extraction),
        h('p', { class: 'muted' }, 'Save using the configuration button above. Changes affect new extraction plans; retries keep the original profile. Entity attributes are unreviewed model annotations, not verified facts.'),
        h('details', null, h('summary', null, 'Effective provider output JSON Schema'), schemaView)),
      h('details', null, h('summary', null, 'Raw configuration'), configView)));
  controls();
  await refreshSystems();
}
