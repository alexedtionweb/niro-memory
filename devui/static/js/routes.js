export const screens = [
  { id: 'workspace', label: 'Memories', views: ['explorer', 'documents', 'intelligence', 'search', 'timeline', 'knowledge-state', 'transitions', 'memory', 'graph'] },
  { id: 'models', label: 'Models & schemas', views: ['embeddings', 'schema', 'tenants'] },
  { id: 'operations', label: 'Operations', views: ['jobs', 'metrics', 'sql', 'rules', 'conversations', 'add', 'memories'] },
];

// Old bookmarks and links inside inspectors resolve into the same three shells.
export function resolveRoute(hash) {
  let raw = hash.replace(/^#/, '') || '/workspace';
  let url;
  try {
    if (/^%2f/i.test(raw)) raw = decodeURIComponent(raw);
    url = new URL(raw, 'http://workspace.local');
  }
  catch { throw new Error('Invalid workspace link. Choose a section from the navigation.'); }
  const [name, ...parts] = url.pathname.replace(/^\//, '').split('/');
  let screen = screens.find(s => s.id === name);
  const view = screen ? url.searchParams.get('view') || screen.views[0] : name;
  screen ||= screens.find(s => s.views.includes(view));
  if (!screen || !screen.views.includes(view)) throw new Error('Unknown view. Choose Memories, Models & schemas, or Operations.');
  let params;
  try { params = parts.length ? parts.map(decodeURIComponent) : url.searchParams.getAll('focus'); }
  catch { throw new Error('Invalid encoded workspace link.'); }
  const focus = params[0] || '';
  const query = new URLSearchParams({ view });
  params.forEach(value => query.append('focus', value));
  return { screen: screen.id, view, focus, params, hash: `#/${screen.id}?${query}`,
    path: `/_dev/views/${screen.id}?${query}` };
}
