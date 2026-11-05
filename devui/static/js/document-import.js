import { call, h, state } from './lib.js';
import { workspaceSession } from './knowledge-ui.js';
import { filePreview } from './file-preview.js';

// Files use two upload lanes; parsing/OCR and model work stay in durable jobs.
export function documentImport(root, project, { media = false } = {}) {
  const active = workspaceSession(root);
  const files = h('input', { type: 'file', multiple: true,
    accept: media ? '.png,.jpg,.jpeg,.wav,.mp3,.m4a,.ogg,.mp4,.webm,.mov' : '.pdf,.txt,.md,.markdown,.docx',
    'aria-label': media ? 'Images, audio or video to interpret' : 'Documents to import' });
  const output = h('div', { 'aria-live': 'polite' });
  const message = h('p', { role: 'status' });
  let busy = false, generation = 0;
  const rows = new Map();
  const previews = new Map();
  const enabled = media ? !!state.info?.capabilities?.media_interpretation : state.info?.capabilities?.document_import !== false;
  function show(result, container) {
    let preview = previews.get(result.id);
    if (!preview) { preview = filePreview(result, active); previews.set(result.id, preview); }
    container.replaceChildren(...[h('strong', null, result.name), h('p', null,
      result.status === 'ingested' ? result.kind === 'media'
        ? `Unreviewed model interpretation saved as ${result.memories.length} searchable text chunk(s). Model: ${result.model}. This is not a verified observation or verbatim transcript.`
        : `${result.pages} page(s) ingested into ${result.memories.length} source-linked chunks. Embeddings and knowledge extraction run separately when enabled; inspect background jobs for progress.`
        : result.status === 'failed' ? 'Import failed.' : result.status === 'processing'
          ? media ? 'Interpreting media in the background…' : 'Parsing document / running local OCR…'
          : ['paused', 'cancelled'].includes(result.status) ? `Import ${result.status}. Manage it in Jobs.` : 'Uploaded. Waiting for the worker.'),
      result.error ? h('p', { role: 'alert', class: 'error-message' }, result.error) : null,
      preview,
      h('a', {href:'#/documents/'+encodeURIComponent(result.id)},'Open document, chunks and lineage'),
      ...(result.memories ?? []).slice(0, 5).map(id => h('a', { href: '#/memory/' + encodeURIComponent(id) }, 'Read imported chunk')),
      h('a', { href: '#/jobs' }, 'Inspect background jobs')].filter(child => child != null));
  }
  async function poll(result, container) {
    if (!active()) return;
    try {
      const current = await call('GET', '/v1/documents/' + encodeURIComponent(result.id));
      if (!active() || rows.get(result.id) !== container) return;
      show(current, container);
      if (['queued', 'processing'].includes(current.status)) {
        const timer = setTimeout(() => { if (active()) void poll(current, container); }, 2000);
        timer.unref?.();
      }
    } catch (e) {
      if (active()) container.append(h('p', { role: 'alert' }, 'Import status unavailable: ' + e.message));
    }
  }
  function track(result) {
    if (!active()) return;
    let container = rows.get(result.id);
    if (!container) {
      container = h('article', { class: 'suggestion' });
      rows.set(result.id, container); output.append(container);
    }
    show(result, container);
    if (['queued', 'processing'].includes(result.status)) void poll(result, container);
  }
  const upload = h('button', { onclick: async () => {
    if (!active() || busy || !enabled) return;
    const selected = Array.from(files.files ?? []);
    if (!selected.length) { message.textContent = 'Choose one or more ' + (media ? 'media files' : 'documents') + ' first.'; return; }
    if (selected.length > 10 || selected.some(f => f.size === 0 || f.size > 20 * 1024 * 1024)) {
      message.textContent = 'Choose up to 10 non-empty files, each no larger than 20 MiB.'; return;
    }
    const formats = media ? /\.(png|jpe?g|wav|mp3|m4a|ogg|mp4|webm|mov)$/i : /\.(pdf|txt|md|markdown|docx)$/i;
    if (selected.some(f => !formats.test(f.name))) {
      message.textContent = media ? 'Supported media: PNG, JPEG, WAV, MP3, M4A, OGG, MP4, WebM and MOV.' : 'Supported formats: PDF, TXT, Markdown and DOCX.'; return;
    }
    busy = true; upload.disabled = files.disabled = true; message.textContent = 'Uploading originals — parsing and OCR run in the background.';
    const user = project.value.trim();
    let next = 0, accepted = 0, failed = 0;
    async function lane() {
      while (next < selected.length && active()) {
        const file = selected[next++];
        try {
          const bytes = new Uint8Array(await file.arrayBuffer());
          if (!active()) return;
          let binary = '';
          for (let i = 0; i < bytes.length; i += 8192) binary += String.fromCharCode(...bytes.subarray(i, i + 8192));
          const result = await call('POST', media ? '/v1/media' : '/v1/documents', { name: file.name, bytes: btoa(binary), user_id: user });
          accepted++;
          track(result);
        } catch (e) {
          failed++;
          if (active()) output.append(h('p', { role: 'alert', class: 'error-message' }, file.name + ': ' + e.message + '. Retry the same file safely.'));
        }
      }
    }
    try {
      await Promise.all([lane(), lane()]);
      if (active()) message.textContent = `${accepted} ${media ? 'media file(s)' : 'document(s)'} accepted; ${failed} upload(s) failed. Ingestion continues independently of this page.`;
    } finally {
      busy = false;
      if (active()) upload.disabled = files.disabled = false;
    }
  } }, media ? 'Interpret media' : 'Import documents');
  if (!enabled) {
    upload.disabled = files.disabled = true;
    message.textContent = media ? 'Media interpretation is disabled. Configure the multimodal model and durable blob storage on the server.'
      : 'Document import is disabled. Enable documents and configure durable blob storage on the server.';
  }
  const refresh = h('button', { onclick: async () => {
    if (!active()) return;
    const token = ++generation;
    try {
      const result = await call('GET', '/v1/documents?user_id=' + encodeURIComponent(project.value.trim()));
      if (!active() || token !== generation) return;
      rows.clear(); previews.clear(); output.replaceChildren();
      const imports = (result.documents ?? []).filter(item => (item.kind === 'media') === media);
      for (const item of imports) track(item);
      if (!imports.length) message.textContent = 'No recent imports in this scope.';
    } catch (e) {
      if (active() && token === generation) message.textContent = 'Import list unavailable: ' + e.message;
    }
  } }, media ? 'Refresh media imports' : 'Refresh imports');
  return h('section', { class: 'card' }, h('h2', null, media ? 'Remember images, audio & video' : 'Import documents'),
    h('p', null, media
      ? `Model: ${state.info?.capabilities?.media_interpretation || 'not configured'}. Interpret images, speech or video with its soundtrack. Up to 10 files per action, 20 MiB each; audio/video at most 60 seconds. Video uses sampled frames, not exhaustive viewing. Originals stay as evidence; interpretations are private, unreviewed model output. Search uses their text, not native media embeddings.`
      : 'PDF, TXT, Markdown or DOCX. Scanned PDFs use local OCR. Up to 10 files per action, 20 MiB each and 100 pages per document. Original files stay as evidence; imported chunks are private to the selected scope.'),
    h('label', null, media ? 'Choose images, audio or video' : 'Choose documents', files), h('div', { class: 'row' }, upload, refresh), message, output);
}
