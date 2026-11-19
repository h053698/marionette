import { formatSize, saveBlob } from './transfer.mjs';
import { confirmDialog, promptDialog } from './dialogs.mjs';

const $ = (id) => document.getElementById(id);
const ROOT = '/sdcard';
const PLACES = [['다운로드', '/sdcard/Download'], ['사진', '/sdcard/DCIM'], ['문서', '/sdcard/Documents'], ['음악', '/sdcard/Music'], ['영상', '/sdcard/Movies']];
const dateFormat = new Intl.DateTimeFormat('ko-KR', { year: 'numeric', month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });

export function setupFiles({ device, notify, onInstalled }) {
  let key = '', epoch = 0, enabled = false, path = ROOT, entries = [], loading = false, busy = 0;
  let abort = new AbortController();
  const dialog = $('files-dialog');

  function crumbs() {
    const parts = path === ROOT ? [] : path.slice(ROOT.length + 1).split('/');
    const nodes = [];
    const add = (label, target) => {
      const button = document.createElement('button');
      button.type = 'button'; button.className = 'crumb'; button.textContent = label;
      button.disabled = target === path; button.addEventListener('click', () => { void list(target); });
      nodes.push(button);
    };
    add('내장 저장공간', ROOT);
    let current = ROOT;
    for (const part of parts) { current += `/${part}`; add(part, current); }
    $('files-path').replaceChildren(...nodes.flatMap((node, index) => (index ? [Object.assign(document.createElement('span'), { textContent: '›', className: 'crumb-sep', ariaHidden: 'true' }), node] : [node])));
  }
  function places() {
    $('files-places').replaceChildren(...PLACES.map(([label, target]) => {
      const button = document.createElement('button');
      button.type = 'button'; button.className = 'place'; button.textContent = label;
      button.setAttribute('aria-pressed', String(path === target || path.startsWith(`${target}/`)));
      button.addEventListener('click', () => { void list(target); });
      return button;
    }));
  }
  function iconFor(entry) {
    const span = document.createElement('span'); span.className = `file-icon${entry.dir ? ' folder' : ''}`; span.setAttribute('aria-hidden', 'true');
    span.textContent = entry.dir ? '' : (entry.name.includes('.') ? entry.name.split('.').pop().slice(0, 4).toUpperCase() : '');
    return span;
  }
  function render() {
    crumbs(); places();
    $('files-up').disabled = path === ROOT || loading;
    $('files-refresh').disabled = loading || !enabled;
    $('files-mkdir').disabled = loading || !enabled || busy > 0;
    $('files-upload').disabled = !enabled;
    const rows = document.createDocumentFragment();
    for (const entry of entries) {
      const row = document.createElement('div'); row.className = 'file-row'; row.setAttribute('role', 'listitem');
      const main = document.createElement(entry.dir ? 'button' : 'div');
      main.className = 'file-main';
      if (entry.dir) { main.type = 'button'; main.addEventListener('click', () => { void list(`${path}/${entry.name}`); }); }
      const copy = document.createElement('span'); copy.className = 'file-copy';
      const name = document.createElement('strong'); name.textContent = entry.name; name.title = entry.name;
      const meta = document.createElement('span');
      meta.textContent = [entry.dir ? '폴더' : formatSize(entry.size), entry.modified ? dateFormat.format(new Date(entry.modified * 1000)) : ''].filter(Boolean).join(' · ');
      copy.append(name, meta); main.append(iconFor(entry), copy);
      const actions = document.createElement('span'); actions.className = 'file-actions';
      const full = `${path}/${entry.name}`;
      const button = (label, handler, danger) => {
        const b = document.createElement('button'); b.type = 'button'; b.className = `file-action${danger ? ' danger' : ''}`; b.textContent = label;
        b.disabled = !enabled; b.addEventListener('click', handler); return b;
      };
      if (!entry.dir) {
        if (entry.name.toLowerCase().endsWith('.apk')) actions.append(button('설치', () => { void install(full, entry); }));
        actions.append(button('받기', () => { void download(full, entry); }));
      }
      actions.append(button('삭제', () => { void askRemove(full, entry); }, true));
      row.append(main, actions); rows.append(row);
    }
    $('file-list').replaceChildren(rows);
    $('files-empty').hidden = loading || entries.length > 0 || !enabled;
    $('files-status').textContent = !enabled ? '기기를 점유하면 파일을 볼 수 있어요.' : loading ? '불러오는 중…' : entries.length ? `${entries.length}개 항목` : '';
  }
  async function list(target = path) {
    if (!enabled) return;
    const version = epoch;
    loading = true; render();
    try {
      const result = await device.request('files', { query: { path: target }, signal: abort.signal });
      if (version !== epoch) return;
      path = result.path || target; entries = Array.isArray(result.entries) ? result.entries : [];
      if (result.truncated) notify('항목이 많아 일부만 표시했어요.');
    } catch (error) {
      if (version !== epoch || error.name === 'AbortError') return;
      notify(error.message || '폴더를 열지 못했어요.');
      if (target !== ROOT && error.status === 404) { path = ROOT; loading = false; void list(ROOT); return; }
    } finally { if (version === epoch) { loading = false; render(); $('file-list').scrollTop = 0; } }
  }
  function track(label) {
    const row = document.createElement('div'); row.className = 'transfer'; row.setAttribute('role', 'status');
    const name = document.createElement('span'); name.textContent = label;
    const bar = document.createElement('span'); bar.className = 'meter'; const fill = document.createElement('span'); bar.append(fill);
    const state = document.createElement('em'); state.textContent = '대기 중';
    row.append(name, bar, state); $('transfers').prepend(row); $('transfers').hidden = false;
    const finish = (text, kind) => {
      state.textContent = text; row.dataset.state = kind;
      setTimeout(() => { row.remove(); if (!$('transfers').children.length) $('transfers').hidden = true; }, kind === 'error' ? 8000 : 3500);
    };
    return { progress(loaded, total) { fill.style.width = `${total ? Math.round((loaded / total) * 100) : 0}%`; state.textContent = loaded >= total ? '기기에 쓰는 중…' : `${Math.round((loaded / total) * 100)}%`; }, indeterminate(text) { fill.style.width = '100%'; fill.classList.add('busy'); state.textContent = text; }, done(text = '완료') { fill.style.width = '100%'; fill.classList.remove('busy'); finish(text, 'ok'); }, error(text) { fill.classList.remove('busy'); finish(text, 'error'); } };
  }
  async function upload(files) {
    if (!enabled || !files.length) return;
    const version = epoch, target = path;
    for (const file of files) {
      if (version !== epoch) return;
      const item = track(file.name);
      if (file.size > 1024 ** 3) { item.error('1GB를 넘는 파일은 올릴 수 없어요'); continue; }
      busy++; render();
      try {
        const name = file.name.replace(/[\\/\u0000-\u001f]/g, '_');
        await device.upload('files', { path: `${target}/${name}` }, file, { onProgress: item.progress, signal: abort.signal });
        if (version !== epoch) return;
        item.done('업로드 완료');
      } catch (error) {
        item.error(error.status === 409 ? '같은 이름의 파일이 있어요' : (error.message || '업로드하지 못했어요'));
        if (error.status === 403 || error.status === 410) return;
      } finally { if (version === epoch) { busy--; render(); } }
    }
    if (version === epoch && path === target) void list(target);
  }
  async function download(full, entry) {
    if (!enabled) return;
    const version = epoch, item = track(entry.name);
    item.indeterminate('받는 중…');
    try {
      const blob = await device.download('download', { path: full }, { signal: abort.signal });
      if (version !== epoch) return;
      saveBlob(blob, entry.name); item.done('받기 완료');
    } catch (error) { item.error(error.message || '받지 못했어요'); }
  }
  async function remove(full, entry) {
    if (!enabled) return;
    const version = epoch;
    try {
      await device.request('files', { method: 'DELETE', query: { path: full }, signal: abort.signal });
      if (version !== epoch) return;
      notify(`${entry.name}을(를) 삭제했어요.`); void list(path);
    } catch (error) { if (version === epoch && error.name !== 'AbortError') notify(error.message || '삭제하지 못했어요.'); }
  }
  async function install(full, entry) {
    if (!enabled) return;
    const version = epoch, item = track(`${entry.name} 설치`);
    item.indeterminate('기기에서 설치하는 중…');
    try {
      const blob = await device.download('download', { path: full }, { signal: abort.signal });
      await device.upload('install', null, blob, { method: 'POST', signal: abort.signal });
      if (version !== epoch) return;
      item.done('설치 완료'); onInstalled?.();
    } catch (error) { item.error(error.message || '설치하지 못했어요'); }
  }
  async function askRemove(full, entry) {
    if (!enabled) return;
    const version = epoch;
    const ok = await confirmDialog({
      title: entry.dir ? '폴더를 삭제할까요?' : '파일을 삭제할까요?',
      message: `'${entry.name}'${entry.dir ? ' 폴더와 안에 있는 모든 항목이' : ' 파일이'} 기기에서 삭제돼요. 되돌릴 수 없어요.`,
      confirmLabel: '삭제', danger: true,
    });
    if (ok && version === epoch) void remove(full, entry);
  }
  async function mkdir() {
    if (!enabled || busy > 0) return;
    const version = epoch, parent = path;
    const clean = (text) => text.replace(/[\\/\u0000-\u001f]/g, '');
    const typed = await promptDialog({
      title: '새 폴더 만들기', message: `${parent === ROOT ? '내장 저장공간' : parent.slice(ROOT.length + 1)} 안에 만들어요.`,
      label: '폴더 이름', placeholder: '예: 테스트 파일', confirmLabel: '만들기',
      validate: (text) => { const name = clean(text); return !name || name === '.' || name === '..' ? '폴더 이름을 입력해 주세요.' : ''; },
    });
    const name = typed && clean(typed);
    if (!name || version !== epoch || !enabled) return;
    try {
      await device.request('files', { method: 'POST', query: { path: `${parent}/${name}` }, signal: abort.signal });
      if (version !== epoch) return;
      void list(path === parent ? parent : path);
    } catch (error) { if (version === epoch && error.name !== 'AbortError') notify(error.status === 409 ? '같은 이름이 이미 있어요.' : (error.message || '폴더를 만들지 못했어요.')); }
  }
  function reset(nextKey) {
    epoch++; abort.abort(); abort = new AbortController(); key = nextKey; path = ROOT; entries = []; loading = false; busy = 0;
    $('transfers').replaceChildren(); $('transfers').hidden = true;
  }

  $('open-files').addEventListener('click', () => {
    if (!enabled || dialog.open) return;
    dialog.showModal(); render();
    if (!entries.length) void list('/sdcard/Download');
  });
  $('close-files').addEventListener('click', () => dialog.close());
  dialog.addEventListener('click', (event) => { if (event.target === dialog) dialog.close(); });
  $('files-up').addEventListener('click', () => { if (path !== ROOT) void list(path.slice(0, path.lastIndexOf('/')) || ROOT); });
  $('files-refresh').addEventListener('click', () => { void list(path); });
  $('files-mkdir').addEventListener('click', () => { void mkdir(); });
  $('files-upload').addEventListener('click', () => $('files-input').click());
  $('files-input').addEventListener('change', () => { const files = [...$('files-input').files]; $('files-input').value = ''; void upload(files); });
  const drop = $('files-drop');
  for (const type of ['dragenter', 'dragover']) drop.addEventListener(type, (event) => { if (!enabled || !event.dataTransfer?.types.includes('Files')) return; event.preventDefault(); drop.classList.add('over'); });
  for (const type of ['dragleave', 'dragend']) drop.addEventListener(type, (event) => { if (!drop.contains(event.relatedTarget)) drop.classList.remove('over'); });
  drop.addEventListener('drop', (event) => { event.preventDefault(); drop.classList.remove('over'); if (enabled) void upload([...event.dataTransfer.files]); });

  return {
    open() { if (enabled && !dialog.open) $('open-files').click(); },
    close() { if (dialog.open) dialog.close(); },
    sync(nextKey, canUse) {
      $('open-files').disabled = !nextKey || !canUse;
      if (nextKey !== key) { reset(nextKey); if (dialog.open) dialog.close(); }
      const next = Boolean(nextKey && canUse);
      if (next !== enabled) { enabled = next; if (dialog.open) render(); }
    },
  };
}
