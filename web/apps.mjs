import { formatSize } from './transfer.mjs';
import { confirmDialog } from './dialogs.mjs';

const $ = (id) => document.getElementById(id);

export function setupApps({ request, notify, onLaunch, device, onNeedFiles }) {
  let key = '', epoch = 0, apps = [], loading = false, launching = false, enabled = false, filter = 'all';
  let processes = [], processLoading = false, processLoaded = false, capturedAt = 0, processTruncated = false;
  let iconQueue = [], iconBusy = false, installing = false;
  const icons = new Map(), queued = new Set(), images = new Map();
  const appsDialog = $('apps-dialog'), processesDialog = $('processes-dialog');
  const observer = new IntersectionObserver((entries) => {
    for (const entry of entries) {
      if (!entry.isIntersecting) continue;
      observer.unobserve(entry.target);
      const name = entry.target.dataset.package;
      if (!icons.has(name) && !queued.has(name)) { queued.add(name); iconQueue.push(name); }
    }
    void loadIcon();
  }, { root: $('app-list'), rootMargin: '48px' });

  function iconElement(app) {
    const icon = document.createElement('span'); icon.className = 'app-icon'; icon.setAttribute('aria-hidden', 'true');
    const fallback = document.createElement('span'); fallback.textContent = (app.label || app.packageName || '?').slice(0, 1).toUpperCase();
    icon.append(fallback);
    if (icons.get(app.packageName)) {
      const image = document.createElement('img'); image.alt = ''; image.src = icons.get(app.packageName); icon.replaceChildren(image);
    } else if (!icons.has(app.packageName)) {
      icon.dataset.package = app.packageName; images.set(app.packageName, icon); observer.observe(icon);
    }
    return icon;
  }
  async function loadIcon() {
    if (iconBusy || !enabled || !appsDialog.open || loading || launching || document.hidden) return;
    const packageName = iconQueue.shift();
    if (!packageName) return;
    const version = epoch;
    iconBusy = true;
    try {
      const result = await request({ action: 'appIcon', packageName });
      if (version !== epoch) return;
      const data = result?.mimeType === 'image/png' && typeof result.data === 'string' && result.data.length <= 100000
        && /^[A-Za-z0-9+/=]+$/.test(result.data) ? `data:image/png;base64,${result.data}` : '';
      icons.set(packageName, data);
      if (data && images.has(packageName)) {
        const image = document.createElement('img'); image.alt = ''; image.src = data;
        images.get(packageName).replaceChildren(image);
      }
    } catch { if (version === epoch) icons.set(packageName, ''); }
    finally {
      iconBusy = false;
      if (version === epoch) queued.delete(packageName);
      if (iconQueue.length) void loadIcon();
    }
  }
  function busyNow() { return loading || launching || installing; }
  function render() {
    const query = $('app-search').value.trim().toLocaleLowerCase();
    const filtered = apps.filter((app) => (filter === 'all' || !app.system) && `${app.label} ${app.packageName}`.toLocaleLowerCase().includes(query));
    const scroll = $('app-list').scrollTop;
    observer.disconnect(); images.clear();
    const fragment = document.createDocumentFragment();
    for (const app of filtered) {
      const row = document.createElement('div'); row.className = 'app-row';
      const open = document.createElement('button');
      open.type = 'button'; open.className = 'app-open'; open.disabled = !enabled || busyNow();
      open.setAttribute('aria-label', `${app.label} 실행`);
      const content = document.createElement('span'); content.className = 'app-copy';
      const name = document.createElement('strong'), detail = document.createElement('span');
      name.textContent = app.label; detail.textContent = app.packageName;
      content.append(name, detail); open.append(iconElement(app), content);
      open.addEventListener('click', () => { void launch(app); });
      row.append(open);
      if (!app.system) {
        const remove = document.createElement('button');
        remove.type = 'button'; remove.className = 'file-action danger'; remove.disabled = !enabled || busyNow();
        remove.textContent = '삭제';
        remove.setAttribute('aria-label', `${app.label} 삭제`);
        remove.addEventListener('click', () => { void askUninstall(app); });
        row.append(remove);
      } else {
        const tag = document.createElement('span'); tag.className = 'app-tag'; tag.textContent = '시스템'; row.append(tag);
      }
      fragment.append(row);
    }
    $('app-list').replaceChildren(fragment);
    $('app-list').scrollTop = scroll;
    $('refresh-apps').disabled = busyNow() || !enabled;
    $('install-apk').disabled = busyNow() || !enabled;
    $('app-search').disabled = !enabled;
    $('process-search').disabled = !enabled;
    $('apps-count').textContent = apps.length || '';
    $('apps-user-count').textContent = apps.length ? apps.filter((app) => !app.system).length : '';
    for (const name of ['all', 'user']) $(`apps-filter-${name}`).setAttribute('aria-pressed', String(filter === name));
    if (!loading && !installing) {
      $('app-status').textContent = !enabled ? '기기를 점유하면 앱을 관리할 수 있어요.'
        : apps.length ? `${filtered.length}개 앱 · 앱을 누르면 기기에서 실행돼요.` : '실행할 수 있는 앱이 없어요.';
    }
    $('refresh-processes').disabled = processLoading || !enabled;
  }
  async function load() {
    if (!enabled || loading || launching) return;
    const version = epoch;
    loading = true; render(); $('app-status').textContent = '앱 목록을 불러오고 있어요.';
    try {
      const result = await request({ action: 'apps' });
      if (version !== epoch) return;
      apps = Array.isArray(result?.apps) ? result.apps : [];
    } catch {
      if (version === epoch) { loading = false; render(); $('app-status').textContent = '앱 목록을 불러오지 못했어요. 새로고침해 주세요.'; }
      return;
    } finally { if (version === epoch) loading = false; }
    if (version === epoch) render();
  }
  function renderProcesses() {
    const query = $('process-search').value.trim().toLocaleLowerCase();
    const filtered = processes.filter((process) => `${process.name} ${process.label || ''} ${process.packageName || ''} ${process.pid} ${process.uid}`.toLocaleLowerCase().includes(query));
    const scroll = $('process-list').scrollTop;
    const fragment = document.createDocumentFragment();
    for (const process of filtered) {
      const row = document.createElement('div'); row.className = 'process-row'; row.setAttribute('role', 'listitem');
      const heading = document.createElement('div'); heading.className = 'process-heading';
      const name = document.createElement('strong'); name.textContent = process.name; name.title = process.name;
      const status = document.createElement('span'); status.className = 'process-state';
      const state = String(process.state || '?');
      const label = ({ R: '실행', S: '대기', D: 'I/O 대기', I: '유휴', T: '정지', Z: '종료 중' })[state[0]] || state;
      status.textContent = label; status.dataset.tooltip = `프로세스 상태: ${state}\n${label}`;
      heading.append(name, status);
      const detail = document.createElement('span'); detail.className = 'process-detail';
      const memory = process.rssKb >= 1024 ? `${(process.rssKb / 1024).toFixed(1)} MB` : `${process.rssKb || 0} KB`;
      detail.textContent = `PID ${process.pid} · UID ${process.uid} · ${memory}`;
      row.append(heading);
      if (process.label) { const app = document.createElement('span'); app.className = 'process-app'; app.textContent = process.label; row.append(app); }
      row.append(detail); fragment.append(row);
    }
    $('process-list').replaceChildren(fragment); $('process-list').scrollTop = scroll;
    $('process-status').textContent = processLoaded
      ? `${filtered.length}개 프로세스${processTruncated ? ' · 목록 일부 표시' : ' · 메모리 사용량 순'}` : '실행 중인 프로세스를 불러오고 있어요.';
    $('process-updated').textContent = capturedAt ? new Date(capturedAt).toLocaleTimeString('ko-KR', { hour12: false }) : '';
    $('process-updated').dateTime = capturedAt ? new Date(capturedAt).toISOString() : '';
    $('refresh-processes').disabled = processLoading || !enabled;
  }
  async function loadProcesses() {
    if (!enabled || processLoading || !processesDialog.open || document.hidden) return;
    const version = epoch;
    processLoading = true; $('refresh-processes').disabled = true;
    if (!processLoaded) $('process-status').textContent = '실행 중인 프로세스를 불러오고 있어요.';
    try {
      const result = await request({ action: 'processes' });
      if (version !== epoch) return;
      processes = Array.isArray(result?.processes) ? result.processes.sort((a, b) => b.rssKb - a.rssKb || a.pid - b.pid) : [];
      capturedAt = Number(result?.capturedAt) || Date.now(); processTruncated = Boolean(result?.truncated); processLoaded = true;
      renderProcesses();
    } catch {
      if (version === epoch) $('process-status').textContent = '프로세스 목록을 불러오지 못했어요. 새로고침해 주세요.';
    } finally { if (version === epoch) { processLoading = false; renderProcesses(); } }
  }
  async function launch(app) {
    if (!enabled || busyNow()) return;
    const version = epoch;
    launching = true; render(); $('app-status').textContent = `${app.label} 실행 중…`;
    try {
      await request({ action: 'launchApp', packageName: app.packageName });
      if (version === epoch) { onLaunch(app.label); notify(`${app.label} 앱을 열었어요.`); }
    } catch (error) { if (version === epoch) notify(error.message || '앱을 실행하지 못했어요.'); }
    finally { if (version === epoch) { launching = false; render(); void loadIcon(); } }
  }
  async function askUninstall(app) {
    if (!enabled || busyNow()) return;
    const version = epoch;
    const ok = await confirmDialog({
      title: `${app.label} 앱을 삭제할까요?`,
      message: `${app.packageName}\n앱과 저장된 데이터가 기기에서 삭제돼요.`,
      confirmLabel: '삭제', danger: true,
    });
    if (ok && version === epoch) void uninstall(app);
  }
  async function uninstall(app) {
    if (!enabled || busyNow()) return;
    const version = epoch;
    installing = true; render(); $('app-status').textContent = `${app.label} 삭제 중…`;
    try {
      await device.request('uninstall', { method: 'POST', body: { packageName: app.packageName }, timeout: 90000 });
      if (version !== epoch) return;
      apps = apps.filter((item) => item.packageName !== app.packageName); icons.delete(app.packageName);
      notify(`${app.label} 앱을 삭제했어요.`);
    } catch (error) { if (version === epoch) notify(error.message || '앱을 삭제하지 못했어요.'); }
    finally { if (version === epoch) { installing = false; render(); } }
  }
  async function installApk(file) {
    if (!enabled || busyNow() || !file) return;
    if (!/\.apk$/i.test(file.name)) { notify('APK 파일만 설치할 수 있어요.'); return; }
    const version = epoch;
    installing = true; render();
    const row = document.createElement('div'); row.className = 'transfer'; row.setAttribute('role', 'status');
    const label = document.createElement('span'); label.textContent = file.name;
    const bar = document.createElement('span'); bar.className = 'meter'; const fill = document.createElement('span'); bar.append(fill);
    const state = document.createElement('em'); state.textContent = '올리는 중';
    row.append(label, bar, state); $('apk-transfers').replaceChildren(row); $('apk-transfers').hidden = false;
    $('app-status').textContent = `${file.name} (${formatSize(file.size)}) 설치 중…`;
    try {
      await device.upload('install', null, file, { method: 'POST', onProgress: (loaded, total) => {
        fill.style.width = `${Math.round((loaded / total) * 100)}%`; state.textContent = loaded >= total ? '기기에서 설치하는 중…' : `${Math.round((loaded / total) * 100)}%`;
      } });
      if (version !== epoch) return;
      fill.style.width = '100%'; state.textContent = '설치 완료'; row.dataset.state = 'ok';
      notify('앱을 설치했어요.');
      installing = false; void load();
    } catch (error) {
      if (version !== epoch) return;
      fill.style.width = '100%'; state.textContent = error.message || '설치하지 못했어요'; row.dataset.state = 'error';
      notify(error.message || '앱을 설치하지 못했어요.');
    } finally {
      if (version === epoch) { installing = false; render(); }
      setTimeout(() => { if (row.isConnected) { row.remove(); if (!$('apk-transfers').children.length) $('apk-transfers').hidden = true; } }, 6000);
    }
  }

  $('open-apps').addEventListener('click', () => {
    if (!enabled || appsDialog.open) return;
    appsDialog.showModal(); render();
    if (!apps.length && !loading) void load(); else void loadIcon();
  });
  $('open-processes').addEventListener('click', () => {
    if (!enabled || processesDialog.open) return;
    processesDialog.showModal(); renderProcesses(); void loadProcesses();
  });
  $('close-apps').addEventListener('click', () => appsDialog.close());
  $('close-processes').addEventListener('click', () => processesDialog.close());
  for (const dialog of [appsDialog, processesDialog]) dialog.addEventListener('click', (event) => { if (event.target === dialog) dialog.close(); });
  appsDialog.addEventListener('close', () => { iconQueue = []; queued.clear(); });
  $('refresh-apps').addEventListener('click', () => { void load(); });
  $('refresh-processes').addEventListener('click', () => { void loadProcesses(); });
  $('app-search').addEventListener('input', () => { $('app-list').scrollTop = 0; render(); });
  $('process-search').addEventListener('input', () => { $('process-list').scrollTop = 0; renderProcesses(); });
  $('apps-filter-all').addEventListener('click', () => { filter = 'all'; render(); });
  $('apps-filter-user').addEventListener('click', () => { filter = 'user'; render(); });
  $('install-apk').addEventListener('click', () => $('apk-input').click());
  $('apk-input').addEventListener('change', () => { const file = $('apk-input').files[0]; $('apk-input').value = ''; void installApk(file); });
  setInterval(() => { if ($('process-auto').checked) void loadProcesses(); }, 5000);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) { void loadProcesses(); void loadIcon(); } });
  return {
    reload() { void load(); },
    sync(nextKey, canUse, subtitle = '') {
      $('device-tools').hidden = !nextKey;
      $('apps-subtitle').textContent = subtitle; $('processes-subtitle').textContent = subtitle;
      if (nextKey !== key) {
        key = nextKey; epoch++; apps = []; processes = []; processLoaded = processLoading = false; capturedAt = 0; filter = 'all';
        icons.clear(); queued.clear(); iconQueue = []; observer.disconnect(); images.clear(); loading = launching = installing = false;
        $('app-search').value = ''; $('app-list').replaceChildren(); $('apk-transfers').replaceChildren(); $('apk-transfers').hidden = true;
        $('process-search').value = ''; $('process-list').replaceChildren(); $('process-updated').textContent = '';
        for (const dialog of [appsDialog, processesDialog]) if (dialog.open) dialog.close();
        enabled = Boolean(key && canUse);
      } else if (enabled !== Boolean(key && canUse)) {
        enabled = Boolean(key && canUse); render();
        if (enabled && appsDialog.open && !apps.length && !loading) void load();
        if (enabled) { void loadProcesses(); void loadIcon(); }
      }
    },
    close() { for (const dialog of [appsDialog, processesDialog]) if (dialog.open) dialog.close(); },
  };
}
