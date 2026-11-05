import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const source = await readFile(new URL('./static/js/routes.js', import.meta.url), 'utf8');
const { screens, resolveRoute } = await import('data:text/javascript;base64,' + Buffer.from(source).toString('base64'));

test('only three primary screens and every supported view has a canonical shell', () => {
  assert.equal(screens.length, 3);
  for (const screen of screens) for (const view of screen.views) {
    const route = resolveRoute('#/' + view);
    assert.equal(route.screen, screen.id);
    assert.equal(route.view, view);
    assert.deepEqual(resolveRoute(route.hash), route);
  }
});

test('old evidence bookmarks preserve encoded names, multiple parameters and query characters', () => {
  const route = resolveRoute('#/conversations/' + encodeURIComponent('run/a?b#c') + '/' + encodeURIComponent('turn/1'));
  assert.deepEqual(route.params, ['run/a?b#c', 'turn/1']);
  assert.deepEqual(resolveRoute(route.hash), route);
  assert.equal(resolveRoute('#%2Fintelligence').view, 'intelligence');
  assert.equal(resolveRoute('').view, 'explorer');
});

test('unknown and cross-workspace modules are rejected before any dynamic import', () => {
  for (const hash of ['#/missing', '#/models?view=sql', '#/workspace?view=../../secret', '#/memory/%E0%A4%A']) {
    assert.throws(() => resolveRoute(hash), /Invalid|Unknown/);
  }
});
