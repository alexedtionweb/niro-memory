// Small helpers shared by all pages: API calls, DOM building, formatting.
// HTMX owns workspace fragments; inspector modules share these DOM helpers.

export const state = {
  get tenant() { return localStorage.getItem('niromem.tenant') || 'dev'; },
  set tenant(v) { localStorage.setItem('niromem.tenant', v); },
  get key() { return sessionStorage.getItem('niromem.key') || ''; },
  set key(v) { sessionStorage.setItem('niromem.key', v); },
  info: null,
};

export function requestHeaders(json) {
  const h = {};
  if (json) h['Content-Type'] = 'application/json';
  if (state.key) h['Authorization'] = 'Bearer ' + state.key;
  else h['X-Tenant'] = state.tenant;
  return h;
}

// call performs an API request and returns parsed JSON or throws.
export async function call(method, path, body) {
  const res = await fetch(path, { method, headers: requestHeaders(body !== undefined), body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) throw new Error((data && data.error) || res.status + ' ' + res.statusText);
  return data;
}

// blobURL fetches a stored part with credentials and returns an object URL.
export async function blobURL(sha) {
  const res = await fetch('/v1/blobs/' + sha, { headers: requestHeaders(false) });
  if (!res.ok) throw new Error('blob ' + res.status);
  return URL.createObjectURL(await res.blob());
}

// h creates an element: h('div', {class: 'x', onclick: fn}, child, [children], 'text').
export function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) {
      if (typeof v !== 'function') throw new TypeError('Event handler must be a function: ' + k);
      el.addEventListener(k.slice(2), guard(v));
    }
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  const add = (c) => {
    if (c === null || c === undefined || c === false) return;
    if (Array.isArray(c)) c.forEach(add);
    else el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  };
  kids.forEach(add);
  return el;
}

export function page(root, title, sub, ...kids) {
  const all = [h('h1', null, title), sub ? h('p', { class: 'sub' }, sub) : null, ...kids.flat()];
  root.replaceChildren(...all.filter((k) => k !== null && k !== undefined && k !== false));
}

export function toast(msg, err) {
  const el = h('div', { class: err ? 'err' : '' }, msg);
  document.getElementById('toast').append(el);
  setTimeout(() => el.remove(), err ? 6000 : 2500);
}

// guard runs an async action and reports failures as a toast.
export function guard(fn) {
  return async (...args) => {
    try { return await fn(...args); } catch (e) { toast(e.message, true); }
  };
}

export function table(cols, rows, onRow) {
  return h('div', { class: 'table-scroll', tabindex: 0, 'aria-label': 'Scrollable data table' }, h('table', null,
    h('thead', null, h('tr', null, cols.map((c) => h('th', null, c.label)))),
    h('tbody', null, rows.map((r) => h('tr', { class: onRow ? 'click' : '', onclick: onRow && (() => onRow(r)) },
      cols.map((c) => h('td', null, c.render ? c.render(r) : fmt(r[c.key]))))))));
}

export function fmt(v) {
  if (v === null || v === undefined || v === '') return '';
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}

export const short = (id) => (id || '').slice(-8);
export const pre = (v) => h('pre', null, typeof v === 'string' ? v : JSON.stringify(v, null, 2));
export const badge = (text, kind) => h('span', { class: 'badge ' + (kind || '') }, text);
export const ms = (ns) => (ns / 1e6).toFixed(2) + ' ms';
export const date = (t) => (t ? new Date(t).toISOString().replace('T', ' ').slice(0, 19) : '');

// field builds a labelled input bound to obj[key].
export function field(label, obj, key, attrs = {}) {
  const tag = attrs.textarea ? 'textarea' : 'input';
  const el = h(tag, { ...attrs, textarea: null, value: obj[key] ?? '' });
  if (attrs.type === 'checkbox') {
    el.checked = !!obj[key];
    el.addEventListener('change', () => (obj[key] = el.checked));
    return h('label', { class: 'check' }, el, label);
  }
  el.addEventListener('input', () => (obj[key] = attrs.type === 'number' ? Number(el.value) : el.value));
  return h('label', null, label, el);
}

export function choose(label, obj, key, options) {
  const el = h('select', null, options.map((o) => h('option', { value: o, selected: obj[key] === o }, o || '(any)')));
  el.addEventListener('change', () => (obj[key] = el.value));
  return h('label', null, label, el);
}

// status describes a memory's temporal state as a badge.
export function status(m) {
  if (m.invalidated_at) return badge('invalidated', 'bad');
  if (m.valid_to && new Date(m.valid_to) <= new Date()) return badge('closed', 'warn');
  if (m.expires_at && new Date(m.expires_at) <= new Date()) return badge('expired', 'warn');
  return badge('current', 'ok');
}

// parseJSON returns undefined for empty input and throws on invalid JSON.
export function parseJSON(text) {
  if (!text || !text.trim()) return undefined;
  return JSON.parse(text);
}
