import { state, call, h, page, badge, date, short, status, guard, toast } from '../lib.js';
import { connections } from '../explorer-model.js';
import { workspaceSession } from '../knowledge-ui.js';
import { tenantCleanup } from '../tenant-cleanup.js';

const palette = { entity: '#14b8a6', version: '#f5ad55', explicit: '#60a5fa', knowledge: '#83d879', inferred: '#b8a0ff', semantic: '#f18fb8' };
const NS = 'http://www.w3.org/2000/svg';
function originLabel(m) {
  if (m.source?.kind === 'media_interpreted') return 'Media interpretation · unreviewed';
  if (m.source?.kind === 'document') return 'Imported document chunk';
  if (m.source?.kind === 'knowledge_enriched') {
    return m.metadata?.enrichment?.review === 'unreviewed' ? 'Model extraction · unreviewed' : 'Model extraction';
  }
  if (m.source?.kind === 'explicit') return m.metadata?.assertion ? 'Saved assertion' : 'Original input';
  return 'Stored memory';
}

function svg(tag, attrs, ...children) {
  const el = document.createElementNS(NS, tag);
  Object.entries(attrs).forEach(([key, value]) => el.setAttribute(key, value));
  children.forEach(child => el.append(child instanceof Node ? child : document.createTextNode(child)));
  return el;
}

function network(memories, links, selected, pick) {
  const width = 900, height = 560;
  const positions = new Map(memories.map((m, i) => {
    const angle = i * Math.PI * (3 - Math.sqrt(5));
    const radius = 30 + 210 * Math.sqrt(i / Math.max(1, memories.length - 1));
    return [m.id, { x: width / 2 + Math.cos(angle) * radius * 1.65, y: height / 2 + Math.sin(angle) * radius }];
  }));
  const drawing = svg('svg', { viewBox: `0 0 ${width} ${height}`, role: 'group', 'aria-label': 'Memory relationship graph', class: 'memory-network' });
  for (const edge of links) {
    const a = positions.get(edge.source_id), b = positions.get(edge.target_id);
    if (!a || !b) continue;
    drawing.append(svg('line', { x1: a.x, y1: a.y, x2: b.x, y2: b.y, stroke: palette[edge.type], 'stroke-width': edge.type === 'version' ? 2 : 1,
      'stroke-dasharray': edge.type === 'inferred' ? '5 5' : 'none',
      opacity: edge.invalidated_at ? 0.25 : !selected || [edge.source_id, edge.target_id].includes(selected) ? 0.8 : 0.12 },
    svg('title', {}, `${edge.relation}${edge.invalidated_at ? ' (retired)' : ''}: ${edge.reason || ''}`)));
  }
  for (const m of memories) {
    const p = positions.get(m.id);
    const count = links.filter(e => e.source_id === m.id || e.target_id === m.id).length;
    const node = svg('g', { tabindex: 0, role: 'button', 'aria-label': m.title || m.text.slice(0, 100), class: 'graph-node' },
      svg('circle', { cx: p.x, cy: p.y, r: m.id === selected ? 15 : 7 + Math.min(count, 7), fill: m.id === selected ? '#f8fafc' : '#6d92ff', stroke: '#1e3152', 'stroke-width': 5 }),
      svg('text', { x: p.x + 17, y: p.y + 4, class: 'node-label' }, (m.title || m.text).slice(0, 25)),
      svg('title', {}, m.text));
    node.addEventListener('click', () => pick(m.id));
    node.addEventListener('keydown', e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); pick(m.id); } });
    drawing.append(node);
  }
  return drawing;
}

export default async function (root, [initialID]) {
  const active = workspaceSession(root);
  let offset = 0, data, selected = initialID || '', semanticLinks = [], showInferred = true;
  const cursors = [''];
  let generation = 0, showHistory = false, deleting = false;
  const query = h('input', { placeholder: 'Filter this slice by text, entity, or slot', 'aria-label': 'Filter memories', class: 'grow' });
  const user = h('input', { placeholder: 'user_id (optional)', 'aria-label': 'User scope' });
  const stage = h('div'), detail = h('section', { class: 'card inspector', tabindex: -1, 'aria-label': 'Selected memory' }), metrics = h('div', { class: 'stats-grid' });
  const count = h('span', { class: 'muted' });
  const listCount = h('p', { class: 'muted' });
  const list = h('div', { class: 'memory-cards' });
  const layout = h('div', { class: 'explorer-layout' }, list, detail);
  const previous = h('button', { onclick: guard(async () => { offset = Math.max(0, offset - 1); await load(); }) }, 'Previous slice');
  const next = h('button', { onclick: guard(async () => {
    const cursor = data.next_cursor || data.memories.at(-1)?.id;
    if (!data.more || !cursor) return;
    cursors[offset + 1] = cursor; offset++; await load();
  }) }, 'Next slice');
  const metric = (label, value, note) => h('div', { class: 'stat' }, h('span', null, label), h('strong', null, value), h('small', null, note));
  const pick = id => { selected = id; semanticLinks = []; render(); if (id) detail.focus(); };
  const semantic = guard(async () => {
    const m = data.memories.find(m => m.id === selected);
    if (!m) return;
    const result = await call('POST', '/v1/search', { query: m.text, user_id: m.user_id || undefined, agent_id: m.agent_id || undefined,
      policy: 'deep', limit: 20, explain: true });
    if (!active()) return;
    // Only label hits as semantic when the vector strategy actually found them.
    semanticLinks = (result.hits || []).filter(hit => hit.memory.id !== m.id && hit.explain?.found_by?.includes('vector'))
      .map(hit => ({ source_id: m.id, target_id: hit.memory.id, type: 'semantic', relation: 'vector neighbor', reason: `Vector signal: ${hit.explain.vector.toFixed(3)}` }));
    const visible = new Set(data.memories.map(m => m.id));
    const shown = semanticLinks.filter(link => visible.has(link.target_id)).length;
    toast(`${shown} vector neighbors in this slice. Inspect Search playground for skipped strategies or out-of-slice results.`);
    render();
  });
  function render() {
    if (!data) return;
    const term = query.value.toLowerCase();
    const matches = data.memories.filter(m => (showHistory || !m.invalidated_at &&
      (!m.valid_to || Date.parse(m.valid_to) > Date.now()) && (!m.expires_at || Date.parse(m.expires_at) > Date.now())) &&
      [m.text, m.title, m.slot, ...(m.entities || []).map(e => e.name)].join(' ').toLowerCase().includes(term));
    const nodes = [...matches.filter(m => m.id === selected), ...matches.filter(m => m.id !== selected)].slice(0, 60), ids = new Set(nodes.map(m => m.id));
    const relationships = (data.relationships || data.inferred_links || []).filter(l => showInferred || l.origin && l.origin !== 'inferred');
    const edges = [...connections(nodes, relationships), ...semanticLinks.filter(e => ids.has(e.source_id) && ids.has(e.target_id))].slice(0, 400);
    stage.replaceChildren(nodes.length ? network(nodes, edges, selected, pick) : h('div', { class: 'empty-state' }, h('h2', null, 'No memories in this view'), h('p', null, 'Save facts through MCP or the Add playground, or change the scope/filter.')));
    count.textContent = `${nodes.length} graph nodes · ${edges.length} connections · ${matches.length} matches in this slice. Graph capped at 60 nodes / 400 edges.`;
    listCount.textContent = `${matches.length} memories shown on this page${showHistory ? ', including older versions' : ' (older versions hidden)'}. ${data.more ? 'More pages available.' : 'End of results.'}`;
    metrics.replaceChildren(metric('Memories in slice', data.memories.length, data.more ? 'More available' : 'End of results'),
      metric('Inferred links', (data.inferred_links || []).length, 'History included; retired edges are dimmed'),
      metric('Embedding model', state.info?.capabilities?.embedder || 'Not configured', 'Optional semantic retrieval'),
      metric('Relationship model', state.info?.capabilities?.relationship_inference || 'Not configured', 'Optional background inference'),
      metric('Knowledge intelligence', state.info?.capabilities?.knowledge_mode || 'disabled', 'Validated transitions, separate from inference'));
    list.replaceChildren(...(matches.length ? matches.map(m => h('button', { class: 'memory-tile ' + (selected === m.id ? 'selected' : ''), onclick: () => pick(m.id) },
      h('strong', null, m.title || m.text.slice(0, 150)),
      h('small', null, originLabel(m)),
      h('span', { class: 'row' }, status(m), h('small', null, m.user_id || 'No project', ' · Version ', m.version)),
      h('small', null, 'Learned ', date(m.recorded_at)))) : [
        h('div', { class: 'empty-state' }, h('h2', null, 'No matching memories here'),
          h('p', null, 'Try another filter, include older versions or add a memory.'),
          h('a', { href: '#/intelligence' }, 'Try a memory'))]));
    const m = data.memories.find(m => m.id === selected);
    detail.hidden = !m;
    layout.setAttribute('class', 'explorer-layout' + (m ? ' has-selection' : ''));
    if (!m) {
      detail.replaceChildren();
      return;
    }
    const linked = [...connections(data.memories, relationships), ...semanticLinks].filter(e => e.source_id === m.id || e.target_id === m.id);
    detail.replaceChildren(...[h('div', { class: 'row' }, h('span', { class: 'eyebrow' }, 'Memory inspector'),
      h('button', { onclick: () => { pick(''); list.querySelector('button')?.focus(); } }, 'Close details')),
      h('h2', null, m.title || short(m.id)),
      h('p', null, m.text), h('p', null, badge(originLabel(m)), status(m), badge(m.kind), badge(`v${m.version}`)),
      m.source?.kind === 'document' ? h('p', { class: 'muted' }, `${m.metadata?.filename || 'Document'} · page ${m.metadata?.page || '?'} · chunk ${m.metadata?.chunk || '?'}`) : null,
      m.source?.kind === 'media_interpreted' ? h('p', { class: 'muted' }, `${m.metadata?.filename || 'Media'} · Model: ${m.source.derived_by || 'not reported'}. This is a model interpretation, not verified evidence. The original media is retained separately.`) : null,
      m.source?.kind === 'knowledge_enriched' ? h('p', { class: 'muted' },
        'This is a model interpretation, not a second manual save. The original input is kept separately as evidence, even when the text is identical.',
        m.source.derived_by ? ` Model: ${m.source.derived_by}.` : '') : null,
      m.source?.kind === 'knowledge_enriched' && m.metadata?.enrichment?.source_id
        ? h('a', { href: '#/memory/' + encodeURIComponent(m.metadata.enrichment.source_id) }, 'Read original input') : null,
      m.subject ? h('p', { class: 'relation-triple' }, `${m.subject} → ${m.metadata?.assertion?.conditions?.polarity === 'negative' ? 'NOT ' : ''}${m.predicate} → ${m.object}`) : null,
      m.metadata?.assertion ? h('p', null, badge(m.metadata.assertion.epistemic_type || 'assertion')) : null,
      m.metadata?.decision ? h('p', null, badge(m.metadata.decision.status || 'unresolved'),
        h('small', null, m.metadata.decision.outcome || '')) : null,
      h('a', { href: '#/memory/' + m.id }, 'Read evidence & version history'),
      h('div', { class: 'row' },
        h('a', { href: '#/knowledge-state/' + encodeURIComponent(m.id) }, 'What else is known?'),
        h('a', { href: '#/intelligence/' + encodeURIComponent(m.id) }, 'Suggest a correction'),
        h('button', { class: 'danger', disabled: deleting, onclick: async () => {
          if (!active() || deleting) return;
          if (!confirm('Permanently delete this memory?\n\n' + m.text.slice(0, 200) +
            '\n\nThis removes this record, not every related memory. To preserve it as history, use Invalidate in the full details page.')) return;
          if (!active()) return;
          deleting = true; render();
          try {
            await call('DELETE', '/v1/memories/' + encodeURIComponent(m.id));
            if (!active()) return;
            if (initialID === m.id) initialID = '';
            if (selected === m.id) selected = '';
            data.memories = data.memories.filter(item => item.id !== m.id);
            render();
            toast('Memory deleted');
            await load();
            if (active() && !selected) list.querySelector('button')?.focus();
          } finally {
            deleting = false;
            if (active()) render();
          }
        } }, deleting ? 'Deleting memory…' : 'Delete memory')),
      h('details', null, h('summary', null, 'Advanced: semantic search'),
        h('button', { onclick: semantic }, 'Find vector neighbors'),
        h('p', { class: 'muted' }, 'Requires a configured embedder. Results are not proof that two claims are equivalent.')),
      m.metadata?.document_id ? h('p', null, h('a', { href: '#/documents/' + encodeURIComponent(m.metadata.document_id) }, 'Open source document, file preview and lineage')) : null,
      h('h2', null, `${linked.length} connections on this page`),
      linked.length ? null : h('p', { class: 'muted' }, 'No connections returned on this page. This is not a full database search.'),
      ...linked.map(edge => {
        const other = data.memories.find(x => x.id === (edge.source_id === m.id ? edge.target_id : edge.source_id));
        return h('div', { class: 'connection-item' },
          badge(edge.type, edge.type === 'inferred' ? 'warn' : ''), h('b', null, edge.relation),
          edge.invalidated_at ? badge('retired', 'warn') : null,
          h('p', { class: 'muted' }, edge.reason || ''),
          edge.origin === 'validated' && edge.operation_key
            ? h('a', { href: '#/transitions/' + encodeURIComponent(edge.operation_key) }, 'Inspect relationship transition') : null,
          edge.type === 'inferred' ? h('small', null, `Model estimate ${(edge.confidence * 100).toFixed(0)}% · ${edge.provider} · ${date(edge.at)}`) : null,
          other ? h('button', { class: 'text-button', onclick: () => pick(other.id) }, other.text.slice(0, 100)) : h('span', null, 'Outside slice'));
      })].filter(child => child != null));
  }
  async function load() {
    if (!active()) return;
    const token = ++generation;
    const result = await call('GET', `/v1/explore?all_versions=true&show_expired=true&limit=100&before_id=${encodeURIComponent(cursors[offset] || '')}&user_id=${encodeURIComponent(user.value.trim())}`);
    if (!active() || token !== generation) return;
    data = { ...result, memories: result.memories ?? [], inferred_links: result.inferred_links ?? [] };
    if (initialID && offset === 0 && !data.memories.some(m => m.id === initialID)) {
      const focused = await call('GET', '/v1/memories/' + encodeURIComponent(initialID));
      if (!user.value.trim() || focused.user_id === user.value.trim()) {
        data.memories.push(focused);
        const linked = await call('GET', `/v1/knowledge/relations/${encodeURIComponent(initialID)}?depth=1&max_edges=40`);
        data.relationships = [...(data.relationships || data.inferred_links), ...linked.relations];
        const known = new Set(data.memories.map(m => m.id));
        const missing = [...new Set(linked.relations.flatMap(l => [l.source_id, l.target_id]))].filter(id => !known.has(id)).slice(0, 40);
        const neighbours = await Promise.all(missing.map(id => call('GET', '/v1/memories/' + encodeURIComponent(id))));
        data.memories.push(...neighbours.filter(m => !user.value.trim() || m.user_id === user.value.trim()));
      }
    }
    if (!active() || token !== generation) return;
    semanticLinks = [];
    previous.disabled = offset === 0;
    next.disabled = !data.more;
    render();
  }
  query.addEventListener('input', () => render());
  const inferenceToggle = h('input', { type: 'checkbox', checked: true });
  inferenceToggle.addEventListener('change', () => { showInferred = inferenceToggle.checked; render(); });
  const historyToggle = h('input', { type: 'checkbox', 'aria-label': 'Include older versions' });
  historyToggle.addEventListener('change', () => { showHistory = historyToggle.checked; render(); });
  page(root, 'Your memories', 'Read what has been remembered. Select a card to understand its history and connections.',
    h('div', { class: 'card' }, h('div', { class: 'row' }, query, user, h('button', { onclick: guard(async () => { offset = 0; await load(); }) }, 'Apply scope / refresh'),
      h('label', { class: 'check' }, historyToggle, 'Include older versions'))),
    listCount, layout,
    h('div', { class: 'row' }, previous, next),
    h('details', { class: 'card' }, h('summary', null, 'Explore connections as a graph'),
      h('p', { class: 'muted' }, 'Model-inferred links are estimates, not confirmed facts. Entity paths are navigation clues. The graph shows only this page, with at most 60 nodes and 400 edges.'),
      h('label', { class: 'check' }, inferenceToggle, 'Show inferred links'),
      h('div', { class: 'legend' }, ...Object.entries(palette).map(([name, color]) => { const dot = h('i'); dot.style.backgroundColor = color; return h('span', null, dot, name); })),
      stage, count),
    h('details', { class: 'card' }, h('summary', null, 'Advanced: models & background processing'), metrics,
      h('button', { onclick: guard(async () => {
        if (!active()) return;
        await call('POST', '/v1/relationships/backfill'); toast('Relationship backfill queued. Monitor Jobs for progress and failures.');
      }) }, 'Backfill inferred relationships'),
      h('a', { href: '#/jobs' }, 'View background jobs')),
    tenantCleanup(active, async () => { selected = ''; offset = 0; await load(); }));
  await load();
}
