// SQL console: read-only queries against the store (SQLite connections
// are opened read-only with query_only; Postgres uses READ ONLY transactions).
import { state, call, h, page, table, guard, fmt } from '../lib.js';

const presets = {
  'Tables (SQLite)': "SELECT name, type FROM sqlite_master WHERE type IN ('table','index') AND name NOT LIKE 'sqlite_%' ORDER BY type, name",
  'Tables (Postgres)': "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() ORDER BY 1",
  'Memories per kind': "SELECT tenant, kind, layer, count(*) AS n FROM memories GROUP BY 1, 2, 3 ORDER BY n DESC",
  'Open slots': "SELECT tenant, user_id, slot, text FROM memories WHERE slot <> '' AND valid_to IS NULL AND invalidated_at IS NULL ORDER BY slot",
  'Jobs by state': "SELECT tenant, kind, state, count(*) AS n FROM jobs GROUP BY 1, 2, 3",
  'Entities': "SELECT tenant, key, type, mentions FROM entities ORDER BY mentions DESC LIMIT 50",
  'Write attempt (must fail)': "DELETE FROM memories",
};

export default async function (root) {
  if (state.info && !state.info.sql) {
    page(root, 'SQL console', 'This store does not offer a SQL console.');
    return;
  }
  const sql = h('textarea', { rows: 6 });
  sql.value = presets['Tables (SQLite)'];
  const out = h('div');
  const run = guard(async () => {
    out.replaceChildren(h('p', { class: 'muted' }, 'Running…'));
    try {
      const r = await call('POST', '/_dev/api/sql', { sql: sql.value, max_rows: 200 });
      out.replaceChildren(h('p', { class: 'muted' }, `${r.rows.length} rows · ${r.took_ms.toFixed(2)} ms (capped at 200)`),
        table(r.columns.map((c, i) => ({ label: c, render: (row) => fmt(row[i]) })), r.rows));
    } catch (e) {
      out.replaceChildren(h('pre', null, e.message));
    }
  });
  sql.addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) run(); });
  page(root, 'SQL console', 'Read-only by construction: writes fail inside the database, not just in this UI. Ctrl+Enter runs.',
    h('div', { class: 'card' },
      h('div', { class: 'row' }, Object.entries(presets).map(([name, q]) => h('button', { onclick: () => { sql.value = q; run(); } }, name))),
      sql, h('div', { class: 'row' }, h('button', { class: 'primary', onclick: run }, 'Run'))),
    out);
}
