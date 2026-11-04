// Deterministic connections are navigation clues, not inferred factual claims.
export function connections(memories, inferred = []) {
  const ids = new Set(memories.map(m => m.id));
  const edges = [];
  const seen = new Set();
  const add = edge => {
    if (!ids.has(edge.source_id) || !ids.has(edge.target_id) || edge.source_id === edge.target_id) return;
    const key = `${edge.type}:${edge.source_id}:${edge.target_id}:${edge.relation || ''}`;
    if (!seen.has(key)) { seen.add(key); edges.push(edge); }
  };
  for (const m of memories) {
    if (m.supersedes) add({ source_id: m.supersedes, target_id: m.id, type: 'version', relation: 'superseded by' });
  }
  const normalize = value => String(value || '').trim().toLowerCase();
  const predicate = m => (m.metadata?.assertion?.conditions?.polarity === 'negative' ? 'NOT ' : '') + (m.predicate || '');
  for (let i = 0; i < memories.length; i++) {
    const a = memories[i];
    for (const b of memories.slice(i + 1)) {
      const names = new Set((a.entities || []).map(e => normalize(e.name)));
      const shared = (b.entities || []).filter(e => names.has(normalize(e.name))).map(e => e.name);
      if (shared.length) add({ source_id: a.id, target_id: b.id, type: 'entity', relation: 'shared entity', reason: shared.join(', ') });
      if (a.object && b.subject && normalize(a.object) === normalize(b.subject)) {
        add({ source_id: a.id, target_id: b.id, type: 'explicit', relation: 'entity path', reason: `${a.subject} → ${predicate(a)} → ${a.object} → ${predicate(b)} → ${b.object}` });
      }
      if (b.object && a.subject && normalize(b.object) === normalize(a.subject)) {
        add({ source_id: b.id, target_id: a.id, type: 'explicit', relation: 'entity path', reason: `${b.subject} → ${predicate(b)} → ${b.object} → ${predicate(a)} → ${a.object}` });
      }
    }
  }
  inferred.forEach(link => add({ ...link, type: !link.origin || link.origin === 'inferred' ? 'inferred' : 'knowledge' }));
  return edges;
}

export function timelineEvents(memories, links = [], mode = 'recorded') {
  const events = [];
  for (const m of memories) {
    events.push({ at: mode === 'valid' ? m.valid_from : m.recorded_at, label: mode === 'valid' ? 'Valid from' : 'Recorded', memory: m });
    if (mode === 'valid' && m.valid_to) events.push({ at: m.valid_to, label: 'Validity ended', memory: m });
    if (mode === 'recorded' && m.closed_at) events.push({ at: m.closed_at, label: 'Validity end learned', memory: m });
    if (m.invalidated_at) events.push({ at: m.invalidated_at, label: 'Corrected / invalidated', memory: m });
    if (m.expires_at) events.push({ at: m.expires_at, label: 'Expiry', memory: m });
  }
  if (mode === 'recorded') for (const link of links) {
    const memory = memories.find(m => m.id === link.source_id);
    if (memory) {
      events.push({ at: link.at, label: !link.origin || link.origin === 'inferred' ? 'Relationship inferred' : 'Knowledge relationship', memory, link });
      if (link.invalidated_at) events.push({ at: link.invalidated_at, label: 'Relationship retired', memory, link });
    }
  }
  return events.filter(e => Number.isFinite(Date.parse(e.at))).sort((a, b) => Date.parse(b.at) - Date.parse(a.at));
}
