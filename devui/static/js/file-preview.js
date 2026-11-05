import { h, requestHeaders } from './lib.js';

// Fetch with normal API credentials; never place API keys in preview URLs.
export function filePreview(source, active) {
  const output = h('div', { class: 'file-preview', 'aria-live': 'polite' });
  let url = '', busy = false, generation = 0;
  const release = () => {
    generation++;
    if (url) URL.revokeObjectURL(url);
    url = '';
    output.replaceChildren();
  };
  const watch = () => {
    const timer = setTimeout(() => {
      if (!active() || !output.isConnected) { release(); return; }
      if (url) watch();
    }, 1000);
    timer.unref?.();
  };
  const preview = h('button', { onclick: async () => {
    if (!active() || busy) return;
    release();
    const token = generation;
    busy = true; preview.disabled = true;
    output.textContent = 'Loading original preview...';
    try {
      const response = await fetch('/v1/documents/' + encodeURIComponent(source.id) + '/preview', { headers: requestHeaders(false) });
      if (!response.ok) {
        const detail = await response.text();
        let message = `Preview unavailable (HTTP ${response.status}). Download the original instead.`;
        try { message = JSON.parse(detail).error || message; } catch { /* Non-JSON proxy errors use the explicit HTTP message. */ }
        throw new Error(message);
      }
      const data = await response.blob();
      if (!active() || token !== generation) return;
      const type = data.type.split(';')[0];
      let viewer;
      if (type === 'text/plain') {
        const text = await data.slice(0, 128 * 1024).text();
        if (!active() || token !== generation) return;
        viewer = h('pre', null, text, data.size > 128 * 1024 ? '\n[Preview limited to first 128 KiB; download for full content.]' : '');
      } else {
        url = URL.createObjectURL(data);
        if (type.startsWith('image/')) viewer = h('img', { src: url, alt: source.name });
        else if (type.startsWith('audio/')) viewer = h('audio', { src: url, controls: true, preload: 'metadata' });
        else if (type.startsWith('video/')) viewer = h('video', { src: url, controls: true, preload: 'metadata' });
        else if (type === 'application/pdf') viewer = h('iframe', { src: url, title: 'PDF preview: ' + source.name, sandbox: '' });
        else throw new Error('Unsupported preview format. Download the original instead.');
        watch();
      }
      output.replaceChildren(viewer, h('button', { onclick: release }, 'Close preview'));
    } catch (error) {
      if (active() && token === generation) {
        release();
        output.append(h('p', { class: 'error-message', role: 'alert' }, error.message));
      }
    } finally { busy = false; preview.disabled = false; }
  } }, 'Preview file');
  const download = h('button', { onclick: async () => {
    if (!active()) return;
    const response = await fetch('/v1/documents/' + encodeURIComponent(source.id) + '/file', { headers: requestHeaders(false) });
    if (!response.ok) throw new Error('Original download failed (HTTP ' + response.status + ').');
    const data = await response.blob();
    if (!active()) return;
    const objectURL = URL.createObjectURL(data);
    const link = h('a', { href: objectURL, download: source.name });
    link.click();
    setTimeout(() => URL.revokeObjectURL(objectURL), 1000);
  } }, 'Download original');
  return h('div', null, h('div', { class: 'row' }, preview, download), output);
}
