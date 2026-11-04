import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
const source = await readFile(new URL('./static/js/explorer-model.js', import.meta.url), 'utf8');
const { connections, timelineEvents } = await import('data:text/javascript;base64,' + Buffer.from(source).toString('base64'));

const original = {
  id: 'old', text: 'Original assertion', kind: 'semantic',
  recorded_at: '2026-01-01T00:00:00Z', valid_from: '2026-01-01T00:00:00Z',
  valid_to: '2026-01-03T00:00:00Z', closed_at: '2026-01-05T00:00:00Z',
};
const replacement = {
  id: 'new', text: 'Current assertion', kind: 'semantic', supersedes: 'old',
  recorded_at: '2026-01-05T00:00:00Z', valid_from: '2026-01-03T00:00:00Z',
};

test('knowledge clock never presents world end as its discovery time', () => {
  const rows = timelineEvents([original, replacement]);
  assert(rows.some(e => e.label === 'Validity end learned' && e.at === original.closed_at));
  assert(!rows.some(e => e.label === 'Validity ended'));
});

test('world clock retains real-world interval boundaries', () => {
  const rows = timelineEvents([original, replacement], [], 'valid');
  assert(rows.some(e => e.label === 'Validity ended' && e.at === original.valid_to));
  assert(!rows.some(e => e.label === 'Validity end learned'));
});

test('derived and validated edges are distinct from model estimates', () => {
  const rows = connections([original, replacement], [
    { source_id: 'new', target_id: 'old', relation: 'supersedes', origin: 'validated' },
    { source_id: 'old', target_id: 'new', relation: 'supports', origin: 'inferred' },
  ]);
  assert(rows.some(e => e.type === 'knowledge' && e.relation === 'supersedes'));
  assert(rows.some(e => e.type === 'inferred' && e.relation === 'supports'));
  assert(rows.some(e => e.type === 'version'));
});

test('out-of-slice and duplicate edges do not create phantom nodes', () => {
  const edge = { source_id: 'old', target_id: 'new', relation: 'supports' };
  assert.equal(connections([original], [edge]).length, 0);
  assert.equal(connections([original, replacement], [edge, edge]).filter(e => e.type === 'inferred').length, 1);
});
