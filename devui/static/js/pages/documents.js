import { call, h, page, badge, date, short, guard, state, pre } from '../lib.js';
import { workspaceSession } from '../knowledge-ui.js';
import { filePreview } from '../file-preview.js';

const colors = { document: '#60a5fa', chunk: '#2dd4bf', enriched: '#c4a0ff', embedding: '#f5ad55' };
const titles = { document: 'Original', chunk: 'Source chunks', enriched: 'Structured knowledge', embedding: 'Embeddings' };
const NS = 'http://www.w3.org/2000/svg';
function svg(tag, attrs, ...children) {
  const el = document.createElementNS(NS, tag);
  Object.entries(attrs).forEach(([key, value]) => el.setAttribute(key, value));
  children.forEach(child => el.append(child instanceof Node ? child : document.createTextNode(child)));
  return el;
}

function provenanceGraph(data, select) {
  const nodes = data.nodes.slice(0, 80), positions = new Map(), types = Object.keys(titles);
  const height = Math.max(320, ...types.map(type => nodes.filter(n => n.type === type).length * 66 + 80));
  const graph = svg('svg', { viewBox: `0 0 1080 ${height}`, role: 'group', 'aria-label': 'Document provenance: original, chunks, knowledge and embeddings', class: 'document-network' });
  types.forEach((type, col) => {
    graph.append(svg('text', { x: 20 + col * 270, y: 25, class: 'document-column-label' }, titles[type]));
    nodes.filter(n => n.type === type).forEach((n, row) => positions.set(n.id, { x: 20 + col * 270, y: 55 + row * 66 }));
  });
  for (const edge of data.edges.slice(0, 200)) {
    const a = positions.get(edge.source_id), b = positions.get(edge.target_id);
    if (!a || !b) continue;
    const x1 = a.x + 212, x2 = b.x;
    graph.append(svg('path', { d: `M${x1} ${a.y + 22} C${(x1 + x2) / 2} ${a.y + 22}, ${(x1 + x2) / 2} ${b.y + 22}, ${x2} ${b.y + 22}`,
      fill: 'none', stroke: edge.origin === 'provenance' ? '#465a76' : '#9d78d4', 'stroke-width': 1.5,
      'stroke-dasharray': edge.origin === 'provenance' ? 'none' : '5 4' }, svg('title', {}, `${edge.relation} (${edge.origin})`)));
  }
  nodes.forEach(n => {
    const p = positions.get(n.id);
    const node = svg('g', { tabindex: 0, role: 'button', 'aria-label': `${titles[n.type]}: ${n.label}`, class: 'graph-node' },
      svg('rect', { x: p.x, y: p.y, width: 212, height: 44, rx: 8, fill: '#171f2d', stroke: colors[n.type], 'stroke-width': 1.5 }),
      svg('text', { x: p.x + 10, y: p.y + 18 }, n.label.slice(0, 30)),
      svg('text', { x: p.x + 10, y: p.y + 34 }, n.type === 'embedding' ? `${n.dims} dims${n.active_model ? ' / query model' : ''}` : n.metadata?.section || short(n.id)),
      svg('title', {}, `${n.label}\n${n.id}`));
    node.addEventListener('click', () => select(n));
    node.addEventListener('keydown', e => { if (['Enter', ' '].includes(e.key)) { e.preventDefault(); select(n); } });
    graph.append(node);
  });
  return h('div', { class: 'document-graph-scroll' }, graph,
    h('p', { class: 'muted' }, `Showing ${nodes.length} nodes, up to 200 edges on this page. Solid links are provenance; dashed links are inferred. Graph capped at 80 nodes. Use the records below for every item on the page.`));
}

export default async function (root, [documentID, initialRevision]) {
  const active = workspaceSession(root);
  const library = h('div', { class: 'document-library' }), content = h('div', { class: 'document-content' });
  const refresh = h('button', { onclick: guard(loadLibrary) }, 'Refresh documents');
  page(root, 'Documents & knowledge lineage', 'An immutable original, versioned derivatives and inspectable jobs. Provenance is not a model-inferred relationship.',
    h('div', { class: 'row' }, refresh, h('a', { href: '#/intelligence' }, 'Import documents'), h('a', { href: '#/graph' }, 'Semantic entity graph')),
    h('div', { class: 'document-layout' }, h('aside', { class: 'card document-library-panel' }, h('h2', null, 'Source library'),
      h('p', { class: 'muted' }, 'Latest 20 imports among 200 recent source episodes. Not an exhaustive export.'), library), content));
  async function loadLibrary() {
    const result = await call('GET', '/v1/documents');
    if (!active()) return;
    library.replaceChildren(...(result.documents?.length ? result.documents.map(d =>
      h('a', { href: '#/documents/' + encodeURIComponent(d.id), class: 'document-library-item' + (d.id === documentID ? ' selected' : '') },
        h('strong', null, d.name), h('span', null, badge(d.status), `${d.pages} pages / ${d.memories?.length || 0} imported chunks`)))
      : [h('p', null, 'No recent documents in this tenant.')]));
  }
  await loadLibrary();
  if (!active()) return;
  if (!documentID) {
    content.append(h('section', { class: 'card empty-state' }, h('h2', null, 'Select a document'),
      h('p', null, 'Open the original file, trace its chunks and embeddings, inspect structured interpretations or create a new processing revision.')));
    return;
  }
  const prefix = '/v1/documents/' + encodeURIComponent(documentID);
  const [original, configuration] = await Promise.all([call('GET', prefix), call('GET', '/_dev/api/knowledge/config')]);
  if (!active()) return;
  let graph, revision = initialRevision || '', after = '', cursorStack = [], requestID = crypto.randomUUID(), busy = false, generation = 0, watcher = 0;
  const metrics = h('div', { class: 'stats-grid' }), controls = h('div', { class: 'row' });
  const diagram = h('div'), records = h('div'), selection = h('div'), revisions = h('div'), error = h('p', { class: 'error-message', role: 'alert' });
  const inspector = h('details', null, h('summary', null, 'Selected node'), selection);
  const revisionSelect = h('select', { 'aria-label': 'Document revision' });
  const jobMessage = h('div', { 'aria-live': 'polite' }), originalCard = h('section', { class: 'card', id: 'document-original' },
    h('p', { class: 'eyebrow' }, '01 / Immutable evidence'), h('h2', null, original.name),
    h('p', { class: 'mono muted' }, original.id), filePreview(original, active));
  const stageNames = { chunks: 'Reparse original / rebuild sections', embeddings: 'Generate embeddings', knowledge: 'Extract structured knowledge', relationships: 'Infer semantic relationships' };
  const knowledge = configuration.effective;
  const stageBoxes = Object.entries(stageNames).map(([stage, label]) => {
    const reason = stage === 'chunks' && original.kind === 'media' ? 'media reinterpretation is not supported'
      : stage === 'chunks' && !state.info?.capabilities?.document_import ? 'document parser not configured'
      : stage === 'knowledge' && (knowledge.mode !== 'enforced' || knowledge.provider === 'deterministic' || knowledge.extraction_mode === 'extractive')
      ? 'requires enforced structured extraction'
      : stage === 'embeddings' && !state.info?.capabilities?.embedder ? 'embedder not configured'
      : stage === 'relationships' && !state.info?.capabilities?.relationship_inference ? 'provider not configured' : '';
    const box = h('input', { type: 'checkbox', value: stage, checked: !reason && ['knowledge', 'embeddings'].includes(stage), disabled: !!reason });
    return { stage, box, label: h('label', { class: 'check' }, box, label, reason ? ` (${reason})` : '') };
  });
  const previous = h('button', { disabled: true, onclick: guard(async () => { after = cursorStack.pop() || ''; await loadGraph(); }) }, 'Previous assets');
  const next = h('button', { disabled: true, onclick: guard(async () => { if (!graph.next_after_id) return; cursorStack.push(after); after = graph.next_after_id; await loadGraph(); }) }, 'Next assets');
  const reprocess = h('button', { class: 'primary', onclick: guard(async () => {
    if (!active() || busy) return;
    const stages = stageBoxes.filter(s => s.box.checked).map(s => s.stage);
    if (!stages.length) { error.textContent = 'Select at least one processing stage.'; return; }
    if (!confirm(`Create a new revision for ${original.name}?\n\nStages: ${stages.join(', ')}.\nValidated partial knowledge may activate with explicit gaps and one targeted repair round. Model interpretations remain unreviewed.`)) return;
    busy = true; reprocess.disabled = true; error.textContent = '';
    try {
      const result = await call('POST', prefix + '/reprocess', { request_id: requestID, stages });
      if (!active()) return;
      requestID = crypto.randomUUID();
      showRevision(result);
      await loadRevisions();
      watchRevision(result.id);
    } finally { busy = false; if (active()) reprocess.disabled = false; }
  }) }, 'Create processing revision');
  function showRevision(r) {
    jobMessage.replaceChildren(h('p', null, badge(r.state, r.state === 'needs_review' || r.state === 'dead' ? 'warn' : ''), short(r.id),
      ' / ', r.stages.join(', ')), r.error ? h('p', { class: 'error-message' }, r.error) : h('p', { class: 'muted' },
      r.state === 'committed' ? (r.incomplete ? 'Partial revision activated: only validated records are searchable; failed portions remain flagged for repair.' : 'Revision committed. The previous revision remains available as history.') : 'Work is durable. Existing active knowledge is unchanged while processing.'),
      r.incomplete ? h('div', { class: 'error-message' }, `${r.issues?.length || 0} source passages need repair.`,
        r.repair_revision_id ? ` Targeted repair requested: ${short(r.repair_revision_id)} (one automatic round; inspect revision history for its current status).` : ' Automatic recovery is exhausted; inspect remaining gaps before requesting more work.',
        h('details', null, h('summary', null, 'Extraction gaps'), pre(r.issues))) : null,
      r.progress ? h('p', null, `Saved checkpoint: ${r.progress.stage} / ${r.progress.completed} of ${r.progress.total}`) : null,
      r.state === 'committed' ? h('p', null, `${r.extracted_facts || 0} claims extracted; ${r.empty_sources || 0} source passages returned no structured records. Empty output is not proof that no knowledge exists.`) : null,
      h('a', { href: '#/jobs' }, 'Inspect / pause / retry jobs'));
  }
  async function watchRevision(id, token = ++watcher) {
    if (!active() || token !== watcher) return;
    try {
      const r = await call('GET', prefix + '/revisions?revision_id=' + encodeURIComponent(id));
      if (!active() || token !== watcher) return;
      showRevision(r);
      if (['committed', 'needs_review', 'dead', 'cancelled', 'paused'].includes(r.state)) {
        await loadRevisions();
        if (r.state === 'committed') { revision = ''; after = ''; cursorStack = []; await loadGraph(); }
        return;
      }
      const timer = setTimeout(() => { if (active() && token === watcher) void watchRevision(id, token); }, 3000);
      timer.unref?.();
    } catch (e) { if (active() && token === watcher) error.textContent = 'Revision status unavailable: ' + e.message + '. Refresh to inspect durable state.'; }
  }
  async function loadRevisions() {
    const result = await call('GET', prefix + '/revisions');
    if (!active()) return;
    const items = result.revisions || [];
    revisionSelect.replaceChildren(h('option', { value: '' }, 'Current committed revision'), h('option', { value: 'original' }, 'Original import'));
    items.filter(r => r.state === 'committed').forEach(r => revisionSelect.append(h('option', { value: r.id }, short(r.id) + ' / ' + date(r.created_at))));
    if (revision && revision !== 'original' && !items.some(r => r.id === revision)) revisionSelect.append(h('option', { value: revision }, short(revision) + ' / requested historical snapshot'));
    revisionSelect.value = revision;
    revisions.replaceChildren(...(items.length ? items.map(r => h('article', { class: 'document-revision-row' }, badge(r.state),
      r.incomplete ? badge('incomplete / needs repair', 'warn') : null, r.repair_of ? badge('targeted repair') : null,
      h('span', { class: 'mono' }, short(r.id)), h('span', null, date(r.created_at)), h('span', null, r.stages.join(' / ')),
      r.state === 'committed' ? h('a', { href: '#/documents/' + encodeURIComponent(documentID) + '/' + encodeURIComponent(r.id) }, 'Inspect snapshot') :
        h('button', { onclick: guard(async () => { await watchRevision(r.id); }) }, 'Inspect status'),
      r.error ? h('p', { class: 'error-message' }, r.error) : null)) : [h('p', { class: 'muted' }, 'No manual revision requests. Original import is the baseline.')]));
  }
  function selectNode(n) {
    inspector.open = true;
    selection.replaceChildren(h('h3', null, titles[n.type]), h('p', null, n.label),
      n.memory_id ? h('a', { href: '#/memory/' + encodeURIComponent(n.memory_id) }, 'Open full record and evidence') : h('a', { href: '#document-original', onclick: e => { e.preventDefault(); originalCard.scrollIntoView({ behavior: 'smooth' }); } }, 'View original file'),
      pre(n));
  }
  async function loadGraph() {
    const token = ++generation;
    const q = new URLSearchParams({ limit: '20' });
    if (revision) q.set('revision_id', revision);
    if (after) q.set('after_id', after);
    const data = await call('GET', prefix + '/graph?' + q);
    if (!active() || token !== generation) return;
    graph = data;
    revisionSelect.value = revision;
    const stat = (title, value, note) => h('div', { class: 'stat' }, h('span', null, title), h('strong', null, value), h('small', null, note));
    metrics.replaceChildren(stat('Revision', (data.is_current ? 'Current' : 'Historical') + (data.revision.incomplete ? ' / incomplete' : ''), short(data.revision.id)),
      stat('Source chunks', data.total_chunks, 'Preserved source evidence'), stat('Enriched items', data.total_enriched, 'Interpretations, not verified facts'),
      stat('Vectors on this page', data.vectors_available ? data.nodes.filter(n => n.type === 'embedding').length : 'Unknown', data.active_model || data.vector_reason || 'No query model'));
    diagram.replaceChildren(provenanceGraph(data, selectNode));
    const assets = data.nodes.filter(n => ['chunk', 'enriched'].includes(n.type));
    records.replaceChildren(h('p', { class: 'muted' }, `${data.returned_items} readable records on this page / ${data.total_items} snapshot references. Semantic edges are shown only when both records are on the page.`),
      ...assets.map(n => h('article', { class: 'document-record' }, h('div', null, badge(titles[n.type]), n.metadata?.section ? badge('Section ' + n.metadata.section) : null,
        n.metadata?.page ? badge('Page ' + n.metadata.page) : null, n.metadata?.review ? badge(n.metadata.review, 'warn') : null,
        n.metadata?.extraction_issues?.length ? badge('needs repair', 'warn') : null,
        n.metadata?.extraction?.status === 'empty' ? badge('no claims extracted', 'warn') : null,
        n.metadata?.valid_to || n.metadata?.invalidated_at ? badge('historical') : null),
        h('a', { href: '#/memory/' + encodeURIComponent(n.memory_id) }, n.label),
        h('div', { class: 'document-vector-tags' }, ...(data.vectors_available ? data.nodes.filter(v => v.type === 'embedding' && v.memory_id === n.memory_id).map(v =>
          h('button', { class: 'text-button', onclick: () => selectNode(v) }, `${v.dims}d / ${v.model}${v.active_model ? ' / query model' : ''}`))
          : [h('span', { class: 'muted' }, data.vector_reason)]),
          data.vectors_available && !data.nodes.some(v => v.type === 'embedding' && v.memory_id === n.memory_id) ? h('span', { class: 'muted' }, 'No persisted embeddings for this record.') : null))));
    previous.disabled = !cursorStack.length;
    next.disabled = !data.next_after_id;
  }
  revisionSelect.addEventListener('change', guard(async () => { revision = revisionSelect.value; after = ''; cursorStack = []; selection.replaceChildren(); await loadGraph(); }));
  controls.append(h('label', null, 'Inspect revision', revisionSelect), h('button', { onclick: guard(async () => { await loadRevisions(); await loadGraph(); }) }, 'Refresh lineage'));
  content.append(originalCard, metrics,
    h('section', { class: 'card' }, h('p', { class: 'eyebrow' }, '02 / Provenance map'), controls, diagram,
      inspector),
    h('section', { class: 'card' }, h('p', { class: 'eyebrow' }, '03 / Inspect derivatives'), records, h('div', { class: 'row' }, previous, next)),
    h('section', { class: 'card' }, h('p', { class: 'eyebrow' }, '04 / Incremental processing'), h('h2', null, 'Create a new revision'),
      h('p', { class: 'muted' }, 'Select the stages to regenerate; other outputs are copied. Rechunking requires embeddings when configured and knowledge when enriched records exist. Validated partial knowledge may activate with visible gaps; one automatic repair round targets only failed chunks. Configuration changes alone never reprocess documents.'),
      h('div', { class: 'document-stage-options' }, ...stageBoxes.map(s => s.label)), reprocess, error, jobMessage,
      h('h3', null, 'Latest 20 processing revisions'), revisions));
  await loadRevisions();
  await loadGraph();
}
