// Metrics: live latency percentiles, throughput and errors per operation,
// with a p95 sparkline kept in the browser.
import { call, h, page, table } from '../lib.js';

const history = new Map(); // op -> recent p95 values

function spark(values) {
  const NS = 'http://www.w3.org/2000/svg';
  const s = document.createElementNS(NS, 'svg');
  s.setAttribute('width', 120); s.setAttribute('height', 24);
  const max = Math.max(...values, 0.001);
  const pts = values.map((v, i) => `${(i / Math.max(values.length - 1, 1)) * 118 + 1},${23 - (v / max) * 21}`).join(' ');
  const p = document.createElementNS(NS, 'polyline');
  p.setAttribute('points', pts); p.setAttribute('fill', 'none'); p.setAttribute('stroke', '#7aa2f7'); p.setAttribute('stroke-width', '1.5');
  s.append(p);
  return s;
}

export default async function (root) {
  const out = h('div');
  page(root, 'Metrics', 'Measured in this process since start. Percentiles are histogram estimates. Prometheus format at /metrics.', out);
  const tick = async () => {
    if (!document.body.contains(out)) return; // page left
    try {
      const rows = await call('GET', '/_dev/api/metrics');
      for (const r of rows) {
        const hst = history.get(r.name) || [];
        hst.push(r.p95_ms);
        if (hst.length > 60) hst.shift();
        history.set(r.name, hst);
      }
      const f = (v) => v.toFixed(2);
      out.replaceChildren(table([
        { label: 'operation', render: (r) => h('b', null, r.name) }, { label: 'count', key: 'count' },
        { label: 'errors', key: 'errors' }, { label: '/s', render: (r) => f(r.per_second) },
        { label: 'p50 ms', render: (r) => f(r.p50_ms) }, { label: 'p95 ms', render: (r) => f(r.p95_ms) },
        { label: 'p99 ms', render: (r) => f(r.p99_ms) }, { label: 'max ms', render: (r) => f(r.max_ms) },
        { label: 'p95 trend', render: (r) => spark(history.get(r.name) || []) },
      ], rows));
    } catch (e) {
      out.replaceChildren(h('pre', null, e.message));
    }
    setTimeout(tick, 2000);
  };
  tick();
}
