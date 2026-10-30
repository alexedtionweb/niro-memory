// Search playground: run retrieval with any policy, see the planner trace,
// per-component score explanations, and the layered Context() preview.
import { state, call, h, page, table, short, badge, ms, field, choose, guard, parseJSON, pre } from '../lib.js';

const q = { query: '', user_id: '', agent_id: '', run_id: '', policy: 'default', limit: 10, threshold: 0, filter: '', valid_at: '', all_versions: false };

function scoreBar(e, policy) {
  if (!e) return null;
  const share = 1 - (e.prior_share ?? 0.2);
  const parts = [['lex', e.lexical], ['vec', e.vector], ['ent', e.entity]].filter(([, v]) => v > 0);
  const total = parts.reduce((s, [, v]) => s + v, 0) || 1;
  const bar = h('div', { class: 'bar' });
  for (const [cls, v] of parts) {
    const span = h('span', { class: cls, title: `${cls} ${v.toFixed(3)}` });
    span.style.width = (100 * share * e.relevance * (v / total)).toFixed(1) + '%';
    bar.append(span);
  }
  const prior = h('span', { class: 'prior', title: `prior ${e.prior.toFixed(3)}` });
  prior.style.width = (100 * (1 - share) * e.prior).toFixed(1) + '%';
  bar.append(prior);
  return bar;
}

export default async function (root) {
  const out = h('div');
  const ctxOut = h('div');
  const req = () => {
    const body = { query: q.query, user_id: q.user_id || undefined, agent_id: q.agent_id || undefined, run_id: q.run_id || undefined,
      policy: q.policy, limit: q.limit, threshold: q.threshold || undefined, explain: true, all_versions: q.all_versions || undefined,
      filter: parseJSON(q.filter) };
    if (q.valid_at) body.valid_at = new Date(q.valid_at).toISOString();
    return body;
  };
  const run = guard(async () => {
    const t0 = performance.now();
    const r = await call('POST', '/v1/search', req());
    const rtt = performance.now() - t0;
    out.replaceChildren(
      h('div', { class: 'card' }, h('h2', null, `Planner trace · engine ${ms(r.took_ns)} · round trip ${rtt.toFixed(1)} ms`),
        table([
          { label: 'strategy', render: (s) => h('b', null, s.strategy) },
          { label: 'ran', render: (s) => (s.ran ? badge('ran', 'ok') : badge('skipped', 'warn')) },
          { label: 'candidates', key: 'candidates' }, { label: 'took', render: (s) => (s.took_ns ? ms(s.took_ns) : '') },
          { label: 'why', key: 'reason' },
        ], r.trace)),
      h('div', { class: 'card' }, h('h2', null, `${r.hits.length} hits`),
        h('div', { class: 'legend' }, h('span', null, h('i', { class: 'lex' }), 'lexical (BM25)'), h('span', null, h('i', { class: 'vec' }), 'vector'),
          h('span', null, h('i', { class: 'ent' }), 'entity'), h('span', null, h('i', { class: 'prior' }), 'prior (recency·confidence·importance)')),
        table([
          { label: 'score', render: (x) => [h('b', null, x.score.toFixed(3)), scoreBar(x.explain)] },
          { label: 'memory', render: (x) => [x.memory.text, h('div', { class: 'muted mono' }, short(x.memory.id), ' ', x.memory.user_id || '')] },
          { label: 'found by', render: (x) => (x.explain?.found_by || []).map((f) => badge(f)) },
          { label: 'signals', render: (x) => x.explain && h('span', { class: 'mono' },
            `lex ${x.explain.lexical.toFixed(2)} vec ${x.explain.vector.toFixed(2)} ent ${x.explain.entity.toFixed(2)} rec ${x.explain.recency.toFixed(2)}`,
            x.explain.entities ? h('div', { class: 'muted' }, 'via ' + x.explain.entities.join(', ')) : null) },
        ], r.hits, (x) => (location.hash = '#/memory/' + x.memory.id))));
  });
  const context = guard(async () => {
    const r = await call('POST', '/v1/context', { query: q.query, user_id: q.user_id || undefined, agent_id: q.agent_id || undefined,
      run_id: q.run_id || undefined, policy: q.policy, turn_tokens: q.run_id ? 400 : 0 });
    const c = r.context;
    ctxOut.replaceChildren(h('div', { class: 'card' },
      h('h2', null, `Context() in ${ms(c.took_ns)}: ${c.profile.length} profile · ${c.scenes.length} scenes · ${c.facts.length} facts · ${c.skills.length} skills${c.truncated ? ' · truncated by budget' : ''}`),
      pre(r.rendered)));
  });
  const policies = (state.info && state.info.policies) || ['default', 'fast', 'voice', 'deep'];
  const input = field('query', q, 'query', { placeholder: 'where does alice live?' });
  input.querySelector('input').addEventListener('keydown', (e) => e.key === 'Enter' && run());
  page(root, 'Search playground', 'No LLM is ever called. The planner runs only the strategies that can help and explains every score.',
    h('div', { class: 'card' },
      h('div', { class: 'row' }, input, field('user_id', q, 'user_id'), field('agent_id', q, 'agent_id'), field('run_id', q, 'run_id')),
      h('div', { class: 'row' }, choose('policy', q, 'policy', policies), field('limit', q, 'limit', { type: 'number' }),
        field('threshold', q, 'threshold', { type: 'number', step: 0.05 }), field('valid at', q, 'valid_at', { placeholder: '2026-01-01' }),
        field('all versions', q, 'all_versions', { type: 'checkbox' })),
      field('filter (JSON)', q, 'filter', { textarea: true, rows: 2 }),
      h('div', { class: 'row' }, h('button', { class: 'primary', onclick: run }, 'Search'), h('button', { onclick: context }, 'Preview Context()'))),
    out, ctxOut);
}
