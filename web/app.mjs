import { clientToDevice } from './protocol.mjs';
import { VideoPlayer } from './video.mjs';
import { apiUrl, fetchCredentials } from './config.mjs';
import { setupTokenSettings } from './tokens.mjs';
import { setupApps } from './apps.mjs';
import { setupFiles } from './files.mjs';
import { createDeviceApi } from './transfer.mjs';
import { setupTooltips } from './tooltips.mjs';
import { setupDeviceHover } from './hover.mjs';
import { setupAgentDashboard } from './agents.mjs';
import { setupUsers } from './users.mjs';
import { RealtimeInput } from './input.mjs';
import { createPicker } from './picker.mjs';
import { setupAssign } from './assign.mjs';
import { choiceDialog, confirmDialog } from './dialogs.mjs';

const $ = (id) => document.getElementById(id);
const state = { session: null, devices: [], tiles: new Map(), filter: 'all', search: '', paused: false, refreshing: false, selected: null, handoffSerial: null, lease: null, assignment: null, restorePending: true, workspaceEpoch: 0, busy: false, choosingRelease: false, adminIntent: null, detailEpoch: 0, detailLive: false };
const maxPreviews = 36;
const filterPicker = createPicker({
  label: '기기 상태 필터', value: 'all',
  options: [{ value: 'all', label: '전체' }, { value: 'available', label: '사용 가능' }, { value: 'occupied', label: '사용 중' }, { value: 'cleanup', label: '정리' }, { value: 'offline', label: '오프라인' }],
  onChange: (value) => { state.filter = value; renderDevices(); },
});
$('filter-picker').append(filterPicker.element);
const clock = (date) => date.toLocaleTimeString('ko-KR', { hour: 'numeric', minute: '2-digit' });
function countdown(ms) {
  const total = Math.max(0, Math.ceil(ms / 1000)), m = Math.floor(total / 60), s = total % 60;
  return m ? `${m}분 ${String(s).padStart(2, '0')}초` : `${s}초`;
}
const tokenSettings = setupTokenSettings({ api, notify });
const agentDashboard = setupAgentDashboard({ api, notify });
const userDirectory = setupUsers({ api, notify });
const deviceApi = createDeviceApi({
  apiUrl, credentials: fetchCredentials,
  target: () => state.lease && state.selected
    ? { base: `/api/devices/${encodeURIComponent(state.selected)}`, token: state.lease.token, csrf: state.session?.csrfToken, key: `${state.selected}:${state.lease.id}` } : null,
});
const appTools = setupApps({ request: async (body) => (await action(body, false))?.result, notify, onLaunch: () => { resetTyped(); }, device: deviceApi });
const fileTools = setupFiles({ device: deviceApi, notify, onInstalled: () => appTools.reload() });
setupTooltips();
let previewTimer, toastTimer, detailPlayer, detailAbort, pointer, detailInput, handoffTimer;
const realtimeActions = new Set(['hover', 'touch', 'tap', 'swipe', 'key', 'text']);
let sessionRequest;
setupDeviceHover($('screen'), {
  current: () => state.lease && state.detailLive && detailInput?.ready && !state.busy
    ? { key: `${state.selected}:${state.lease.id}`, metadata: detailPlayer?.metadata, touching:Boolean(pointer) } : null,
  send: (body) => {
    if (body.phase === 'move') detailInput?.move(body);
    else void detailInput?.request(body).catch(() => {});
  },
});
const isAdmin = () => state.session?.user?.role === 'admin';
const devicePath = (serial = state.selected) => `/api/devices/${encodeURIComponent(serial)}`;
const previewPath = (serial) => `/api/admin/devices/${encodeURIComponent(serial)}/preview`;
const modelName = (device) => (device.name || device.model || 'Android').replaceAll('_', ' ');
const specLine = (device) => [device.androidVersion ? `Android ${device.androidVersion}` : '', device.width && device.height ? `${device.width}×${device.height}` : ''].filter(Boolean).join(' · ');
const deviceLabel = (device) => `${device.rawSerial || device.serial}${device.agentId ? ` · ${device.agentId}` : ''}`;
const cleanupLabels = {
  cleaning: { label: '정리 중', hint: '정리를 마치면 다른 사용자가 기기를 쓸 수 있어요.' },
  cleaning_failed: { label: '정리 실패', hint: '관리자가 정리를 다시 시도하거나 기기를 복귀시킬 수 있어요.' },
};
const unavailableLabels = { unauthorized: '인증 필요', usb: '디버깅 대기', disconnected: '연결 해제됨', reconnecting: '재연결 중' };
const stateLabel = (device) => cleanupLabels[device.state]?.label || unavailableLabels[device.state] || (device.state !== 'device' ? '오프라인' : device.occupied ? '사용 중' : '사용 가능');
const viewKey = 'marionette-device-view';
function rememberAssignment(serial, lease) {
  state.assignment = { serial, lease };
  if (state.selected === serial) state.lease = lease;
  if (state.selected === serial && $('device-dialog').open) {
    try { sessionStorage.setItem(viewKey, JSON.stringify({ subject: state.session.user.subject, serial, leaseId: lease.id })); } catch {}
  }
}
function clearView() { try { sessionStorage.removeItem(viewKey); } catch {} }

async function restoreAssignment(device) {
  const epoch = state.workspaceEpoch;
  const current = () => state.workspaceEpoch === epoch && state.session?.authenticated;
  try {
    // The public lease ID is a precondition: restoring must never allocate a replacement.
    let lease = await api(`${devicePath(device.serial)}/lease`, { method: 'POST', body: { ttlSeconds: 300, leaseId: device.leaseId } });
    if (!current()) return;
    lease = await api(`${devicePath(device.serial)}/lease`, { method: 'PUT', body: { ttlSeconds: 300 }, token: lease.token });
    if (!current() || state.assignment) return;
    rememberAssignment(device.serial, lease);
    await api(`${devicePath(device.serial)}/start`, { method: 'POST', token: lease.token, timeout: 45000 });
    if (!current() || state.assignment?.lease.id !== lease.id) return;
    let view;
    try { view = JSON.parse(sessionStorage.getItem(viewKey)); } catch {}
    const reopen = view?.subject === state.session.user.subject && view.serial === device.serial && view.leaseId === lease.id;
    if (reopen && !$('device-dialog').open) openDevice(device.serial);
    updateDetail();
  } catch (error) {
    if (current() && error.status !== 401) notify(`배정된 기기에 다시 연결하지 못했어요. ${error.message}`);
  }
}

function notify(message) {
  const toast = $('toast');
  toast.textContent = message;
  toast.hidden = false;
  if (toast.showPopover) { if (toast.matches(':popover-open')) toast.hidePopover(); toast.showPopover(); }
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { toast.hidden = true; if (toast.hidePopover && toast.matches(':popover-open')) toast.hidePopover(); }, 5000);
}

async function api(path, { method = 'GET', body, token, timeout = 20000, signal, keepalive = false } = {}) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (token) headers['X-Lease-Token'] = token;
  if (!['GET', 'HEAD'].includes(method) && state.session?.csrfToken) headers['X-CSRF-Token'] = state.session.csrfToken;
  const response = await fetch(apiUrl(path), { method, headers, credentials: fetchCredentials, body: body === undefined ? undefined : JSON.stringify(body),
    signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(timeout)]) : AbortSignal.timeout(timeout), keepalive });
  const result = await response.json().catch(() => ({}));
  if (!response.ok || result.ok === false) {
    const error = new Error(result.error || `요청에 실패했어요 (${response.status}).`);
    error.status = response.status;
    if (response.status === 401) void checkSession();
    throw error;
  }
  return result;
}

async function checkSession() {
  if (sessionRequest) return sessionRequest;
  sessionRequest = (async () => {
    try {
      const session = await api('/api/session');
      if (!session.authenticated) {
        stopWorkspace();
        state.session = session;
        $('app-view').hidden = true;
        $('login-view').hidden = false;
        $('login-button').setAttribute('aria-disabled', String(!session.configured));
        $('login-button').tabIndex = session.configured ? 0 : -1;
        $('login-button').href = apiUrl(session.loginURL || '/auth/login');
        $('login-hint').textContent = session.configured ? '안전한 회사 로그인 페이지로 이동합니다.' : '로그인 설정을 기다리고 있어요.\n관리자에게 SSO 연결을 요청하세요.';
        return;
      }
      const changed = !state.session?.authenticated || state.session.user?.subject !== session.user?.subject || state.session.user?.role !== session.user?.role;
      if (changed) stopWorkspace();
      state.session = session;
      $('login-view').hidden = true;
      $('app-view').hidden = false;
      $('profile-name').textContent = session.user.name || session.user.email || '사용자';
      $('rail-avatar').textContent = (session.user.name || session.user.email || 'M').slice(0, 1).toUpperCase();
      $('role-badge').textContent = isAdmin() ? '관리자' : '멤버';
      $('page-title').firstChild.textContent = isAdmin() ? '기기 모니터링 ' : '모바일 기기 ';
      if (!$('devices-view').hidden) $('breadcrumb-label').textContent = isAdmin() ? '기기 모니터링' : '모바일 기기';
      $('page-description').textContent = isAdmin() ? '연결된 모든 기기를 한눈에 확인하세요.' : '한 번에 한 기기를 배정받아 사용합니다. 내 기기만 표시됩니다.';
      $('assign-device').hidden = isAdmin();
      $('filter-picker').hidden = !isAdmin();
      $('toggle-live').hidden = !isAdmin();
      $('app-view').classList.toggle('member-workspace', !isAdmin());
      document.querySelector('.density-label').hidden = !isAdmin();
      $('device-board').setAttribute('aria-label', isAdmin() ? '모바일 기기 화면' : '사용할 모바일 기기');
      agentDashboard.setAdmin(isAdmin());
      userDirectory.setAdmin(isAdmin());
      if (changed) await refresh();
    } catch {
      $('app-view').hidden = true;
      $('login-view').hidden = false;
      $('login-button').setAttribute('aria-disabled', 'true');
      $('login-hint').textContent = '서버에 연결할 수 없어요. 잠시 후 새로고침해 주세요.';
      stopWorkspace();
      state.session = null;
    } finally { $('loading').hidden = true; }
  })().finally(() => { sessionRequest = null; });
  return sessionRequest;
}

function stopWorkspace() {
  assignChooser.close();
  agentDashboard.stop();
  userDirectory.stop();
  tokenSettings.close();
  appTools.sync('', false);
  fileTools.sync('', false);
  clearTimeout(previewTimer);
  previewTimer = null;
  for (const tile of state.tiles.values()) { stopTile(tile); observer.unobserve(tile.element); }
  state.tiles.clear();
  $('device-grid').replaceChildren();
  detailAbort?.abort();
  detailPlayer?.close();
  closeInput();
  detailPlayer = null;
  state.detailEpoch++;
  state.lease = null;
  state.assignment = null;
  state.restorePending = true;
  state.workspaceEpoch++;
  state.selected = null;
  state.handoffSerial = null;
  state.devices = [];
  state.busy = false;
  state.adminIntent = null;
  pointer = null;
  if ($('device-dialog').open) $('device-dialog').close();
}

function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', `#i-${name}`);
  svg.append(use);
  svg.setAttribute('aria-hidden', 'true');
  return svg;
}

function createTile(device) {
  if (!isAdmin()) return createMemberTile(device);
  const element = document.createElement('article');
  element.className = 'device-tile';
  const head = document.createElement('div'); head.className = 'tile-head';
  const dot = document.createElement('span'); dot.className = 'tile-status-dot';
  const serial = document.createElement('span'); serial.className = 'tile-serial'; serial.textContent = serial.title = deviceLabel(device);
  const openIcon = icon('open'); openIcon.classList.add('open-icon');
  head.append(dot, serial, openIcon);
  const button = document.createElement('button'); button.className = 'tile-open';
  const canvas = document.createElement('canvas'); canvas.hidden = true;
  const placeholder = document.createElement('span'); placeholder.className = 'tile-placeholder';
  const hint = document.createElement('span'); placeholder.append(icon('phone'), hint);
  const status = document.createElement('span'); status.className = 'tile-status';
  button.append(canvas, placeholder, status);
  button.addEventListener('click', () => openDevice(device.serial));
  const foot = document.createElement('div'); foot.className = 'tile-foot';
  const model = document.createElement('span'); model.className = 'tile-model';
  const owner = document.createElement('span'); owner.className = 'tile-owner';
  foot.append(model, owner); element.append(head, button, foot);
  const tile = { element, canvas, placeholder, hint, status, button, model, owner, serialEl: serial, serial: device.serial, player: null, visible: false, live: false };
  state.tiles.set(device.serial, tile);
  observer.observe(element);
  return tile;
}

function createMemberTile(device) {
  const element = document.createElement('article'); element.className = 'device-tile member-device';
  const button = document.createElement('button'); button.className = 'member-open';
  const head = document.createElement('span'); head.className = 'member-device-head';
  const symbol = document.createElement('span'); symbol.className = 'member-device-icon'; symbol.append(icon('phone'));
  const identity = document.createElement('span'); identity.className = 'member-device-identity';
  const model = document.createElement('strong'); model.className = 'member-model';
  const serial = document.createElement('span'); serial.className = 'tile-serial'; serial.textContent = serial.title = deviceLabel(device);
  identity.append(model, serial);
  const status = document.createElement('span'); status.className = 'member-status';
  head.append(symbol, identity, status);
  const foot = document.createElement('span'); foot.className = 'member-device-foot';
  const owner = document.createElement('span'); owner.className = 'member-owner';
  const action = document.createElement('span'); action.className = 'member-device-action';
  const hint = document.createElement('span'); action.append(hint, icon('arrow'));
  foot.append(owner, action); button.append(head, foot); element.append(button);
  button.addEventListener('click', () => openDevice(device.serial));
  const tile = { element, button, model, owner, hint, status, serialEl: serial, serial: device.serial, player: null, visible: false, live: false };
  state.tiles.set(device.serial, tile);
  return tile;
}

function matches(device) {
  if (state.search && !`${device.rawSerial || device.serial} ${device.agentId || ''} ${modelName(device)}`.toLocaleLowerCase().includes(state.search)) return false;
  if (state.filter === 'available') return device.state === 'device' && !device.occupied;
  if (state.filter === 'occupied') return device.state === 'device' && device.occupied;
  if (state.filter === 'cleanup') return Boolean(cleanupLabels[device.state]);
  if (state.filter === 'offline') return device.state !== 'device' && !cleanupLabels[device.state];
  return true;
}

function renderDevices() {
  const counts = { all: state.devices.length, available: 0, occupied: 0, offline: 0, cleanup: 0 };
  for (const device of state.devices) counts[cleanupLabels[device.state] ? 'cleanup' : device.state !== 'device' ? 'offline' : device.occupied ? 'occupied' : 'available']++;
  filterPicker.setOptions([{ value: 'all', label: `전체 ${counts.all}` }, { value: 'available', label: `사용 가능 ${counts.available}` }, { value: 'occupied', label: `사용 중 ${counts.occupied}` }, { value: 'cleanup', label: `정리 ${counts.cleanup}` }, { value: 'offline', label: `오프라인 ${counts.offline}` }]);
  $('device-count').textContent = String(counts.all);
  $('assign-device').disabled = Boolean(state.assigning || state.assignment || counts.all);
  $('assign-device').textContent = state.assigning ? '기기 배정 중…' : counts.all ? '내 기기 배정됨' : '기기 배정받기';
  const serials = new Set(state.devices.map((device) => device.serial));
  for (const [serial, tile] of state.tiles) {
    if (!serials.has(serial)) { stopTile(tile); observer.unobserve(tile.element); tile.element.remove(); state.tiles.delete(serial); }
  }
  let visible = 0;
  for (const device of state.devices) {
    const tile = state.tiles.get(device.serial) || createTile(device);
    tile.element.dataset.state = device.state;
    tile.element.dataset.occupied = String(Boolean(device.occupied));
    tile.element.hidden = !matches(device);
    if (!tile.element.hidden) visible++;
    tile.button.setAttribute('aria-label', `${modelName(device)} ${deviceLabel(device)} · ${stateLabel(device)} · ${isAdmin() ? '화면 열기' : '기기 열기'}`);
    tile.model.textContent = modelName(device);
    tile.model.title = modelName(device);
    tile.serialEl.textContent = tile.serialEl.title = [deviceLabel(device), specLine(device)].filter(Boolean).join(' · ');
    tile.owner.textContent = device.occupied ? device.owner || '사용 중' : isAdmin() ? '' : device.state === 'device' ? '바로 연결할 수 있어요' : device.state === 'unauthorized' ? '기기에서 USB 디버깅을 허용해 주세요' : device.state === 'usb' ? 'USB 연결됨 · 디버깅 연결을 확인해 주세요' : '기기 연결을 확인해 주세요';
    if (!isAdmin() && device.occupied && device.expiresAt) tile.owner.textContent += ` · ${new Date(device.expiresAt).toLocaleTimeString('ko-KR', { hour: '2-digit', minute: '2-digit' })}까지`;
    tile.owner.title = device.owner || '';
    tile.hint.textContent = isAdmin() ? device.state === 'device' ? '화면 연결 대기' : stateLabel(device) : device.state !== 'device' ? '연결 상태' : device.occupied ? '사용 정보' : '기기 열기';
    if (!tile.player) tile.status.textContent = device.state === 'device' && isAdmin() ? state.paused ? '일시정지' : '대기 중' : stateLabel(device);
    if (!tile.element.parentElement) $('device-grid').append(tile.element);
  }
  $('board-empty').hidden = visible > 0;
  $('empty-title').textContent = counts.all ? '조건에 맞는 기기가 없어요' : isAdmin() ? '연결된 기기가 없어요' : '배정된 기기가 없어요';
  $('empty-description').textContent = counts.all ? '검색어나 상태 필터를 바꿔 보세요.' : isAdmin() ? '기기를 연결한 뒤 다시 확인해 주세요.' : '기기 배정받기를 누르면 사용 가능한 기기 한 대를 연결합니다.';
  updateSummary();
  updateDetail();
  updateLeaseClock();
  queuePreviews();
}

async function refresh() {
  if (state.refreshing || !state.session?.authenticated) return;
  state.refreshing = true;
  const assignmentAtRefresh = state.assignment;
  const workspaceEpoch = state.workspaceEpoch;
  $('refresh').disabled = true;
  try {
    const [status, result] = await Promise.all([api('/api/status'), api('/api/devices')]);
    if (!state.session?.authenticated || workspaceEpoch !== state.workspaceEpoch) return;
    const ready = isAdmin() ? status.adbAvailable && status.deviceJarAvailable : status.connected;
    $('host-status').className = `host-status ${ready ? 'ready' : 'error'}`;
    $('host-label').textContent = !isAdmin() ? '서버 연결됨' : !status.agentCount ? '에이전트 연결 대기' : !status.adbAvailable ? 'ADB 확인 필요' : !status.deviceJarAvailable ? '드라이버 준비 필요' : `에이전트 ${status.agentCount}대 연결됨`;
    const previousState = state.devices.find((device) => device.serial === state.selected)?.state;
    state.devices = (result.devices || []).sort((a, b) => a.serial.localeCompare(b.serial, undefined, { numeric: true }));
    if (state.assignment && state.assignment.lease.token === assignmentAtRefresh?.lease.token && !state.busy) {
      const device = state.devices.find((item) => item.serial === state.assignment.serial);
      if (!device || device.state !== 'device') endLease('기기 연결이 끊어졌어요', 'USB 케이블과 기기 상태를 확인하세요.');
      else if (!device.mine || !device.occupied || device.leaseId !== state.assignment.lease.id) endLease();
    }
    renderDevices();
    if (state.restorePending) {
      state.restorePending = false;
      const assigned = state.devices.find((device) => device.mine && device.occupied && device.leaseId);
      if (assigned && !state.assignment) await restoreAssignment(assigned);
    }
    if (workspaceEpoch !== state.workspaceEpoch) return;
    syncDetail(previousState);
    $('last-updated').textContent = `${new Date().toLocaleTimeString('ko-KR', { hour12: false })} 업데이트`;
  } catch (error) {
    if (error.status !== 401) {
      $('host-status').className = 'host-status error';
      $('host-label').textContent = '서버 연결 확인 필요';
      if (!state.devices.length) {
        $('board-empty').hidden = false;
        $('empty-title').textContent = '기기 목록을 불러오지 못했어요';
        $('empty-description').textContent = '서버와 에이전트 연결을 확인하고 새로고침해 주세요.';
      }
    }
  } finally { state.refreshing = false; $('refresh').disabled = false; }
}

const observer = new IntersectionObserver((entries) => {
  for (const entry of entries) {
    const tile = [...state.tiles.values()].find((item) => item.element === entry.target);
    if (tile) tile.visible = entry.isIntersecting;
  }
  queuePreviews();
}, { rootMargin: '100px' });

function queuePreviews(delay = 0) {
  if (previewTimer != null) return;
  previewTimer = setTimeout(() => { previewTimer = null; reconcilePreviews(); }, delay);
}

function stopTile(tile) {
  tile.player?.close();
  tile.player = null;
  tile.live = false;
  tile.status.dataset.live = 'false';
  tile.status.textContent = cleanupLabels[tile.element.dataset.state]?.label || (state.paused || document.hidden ? '일시정지' : '대기 중');
}

function reconcilePreviews() {
  const enabled = isAdmin() && !state.paused && !document.hidden && !$('device-dialog').open;
  const candidates = [...state.tiles.values()].filter((tile) => enabled && tile.visible && !tile.element.hidden && tile.element.dataset.state === 'device').slice(0, maxPreviews);
  const wanted = new Set(candidates);
  for (const tile of state.tiles.values()) if (tile.player && !wanted.has(tile) && tile.serial !== state.handoffSerial) stopTile(tile);
  // Stagger driver starts so a large farm does not launch every ADB process at once.
  const next = candidates.find((tile) => !tile.player);
  if (next) {
    let lastKeyframe = -Infinity;
    next.player = new VideoPlayer(next.canvas, {
      onState: (status, message) => {
        next.live = status === 'live';
        next.status.dataset.live = String(next.live);
        next.status.textContent = ({ connecting: '연결 중', configuring: '화면 준비 중', live: 'LIVE', retrying: '재연결 중', error: '연결 확인' })[status];
        next.status.title = message || '';
        if (status === 'error' && next.canvas.hidden) next.hint.textContent = message || '화면 연결을 확인하세요';
        updateSummary();
      },
      onFrame: () => { next.canvas.hidden = false; next.placeholder.hidden = true; },
      requestKeyframe: () => {
        if (performance.now() - lastKeyframe < 800) return;
        lastKeyframe = performance.now();
        void api(`/api/admin/devices/${encodeURIComponent(next.serial)}/keyframe`, { method: 'POST' }).catch(() => {});
      },
      onUnauthorized: () => { void checkSession(); },
    });
    next.player.start(previewPath(next.serial));
    if (candidates.some((tile) => !tile.player)) queuePreviews(160);
  }
  updateSummary();
}

function updateSummary() {
  const online = state.devices.filter((device) => device.state === 'device').length;
  const live = [...state.tiles.values()].filter((tile) => tile.live).length;
  $('board-summary').textContent = isAdmin() ? state.paused ? `${online}대 연결 · 실시간 화면 일시정지` : `${online}대 연결 · ${live}대 실시간` : `${online}대 연결 · 기기를 열어 테스트를 시작하세요`;
  document.querySelector('.live-dot').classList.toggle('paused', state.paused || !live);
}

function placeholder(message, hint = '') {
  $('screen-placeholder').hidden = false;
  $('screen').hidden = true;
  $('live-badge').hidden = true;
  $('screen-message').textContent = message;
  $('screen-hint').textContent = hint;
  state.detailLive = false;
  pointer = null;
  controls();
}

function updateDetail() {
  if (!state.selected) return;
  const device = state.devices.find((item) => item.serial === state.selected);
  $('device-title').textContent = device ? modelName(device) : '연결이 해제된 기기';
  $('device-detail').textContent = device ? [deviceLabel(device), specLine(device)].filter(Boolean).join(' · ') : state.selected;
  $('detail-state').textContent = device ? stateLabel(device) : '오프라인';
  const cleanup = cleanupLabels[device?.state];
  const tone = cleanup ? device.state : !device || device.state !== 'device' ? 'offline' : device.occupied ? 'busy' : '';
  $('detail-state').className = `device-state ${tone}`;
  $('connection-hint').textContent = cleanup?.hint || (!device || device.state !== 'device'
    ? device?.state === 'usb' ? 'USB 연결은 감지됐어요. ADB가 연결되면 점유하고 조작할 수 있어요.' : device?.state === 'unauthorized' ? '폰에서 이 컴퓨터의 USB 디버깅을 허용해 주세요.' : '기기가 다시 연결되면 점유하고 조작할 수 있어요.'
    : state.lease ? '이 기기를 조작할 수 있어요. 사용 후 반납해 주세요.' : device.mine ? '이미 배정된 기기예요. 조작 이어가기로 연결하세요.' : device.occupied ? `${device.owner || '다른 사용자'} 님이 사용 중이에요.${isAdmin() ? ' 현재 화면은 읽기 전용입니다.' : ' 반납 후 연결할 수 있어요.'}` : isAdmin() ? '현재 화면은 읽기 전용입니다. 점유하면 조작할 수 있어요.' : '점유하면 실시간 화면과 조작이 시작됩니다.');
  controls();
}

function controls() {
  const device = state.devices.find((item) => item.serial === state.selected);
  const active = Boolean(state.lease && !state.busy && state.detailLive && detailInput?.ready);
  const manageable = isAdmin() && device?.occupied && !state.lease && Boolean(device.leaseId);
  const toolKey = state.lease ? `${state.selected}:${state.lease.id}` : '';
  const toolSubtitle = device ? `${modelName(device)} · ${deviceLabel(device)}` : '';
  appTools.sync(toolKey, !state.busy, toolSubtitle);
  fileTools.sync(toolKey, !state.busy);
  $('connect').disabled = state.busy || !device || device.state !== 'device' || Boolean(state.lease) || Boolean(device.occupied && !device.mine);
  $('connect').textContent = state.busy ? '연결 중…' : device?.mine ? '조작 이어가기' : '점유하고 조작';
  $('connect').hidden = Boolean(state.lease) || Boolean(manageable) || Boolean(cleanupLabels[device?.state]);
  $('release').hidden = !state.lease;
  $('release').disabled = state.busy;
  $('admin-cleanup').hidden = !isAdmin() || device?.state !== 'cleaning_failed';
  $('admin-cleanup-retry').disabled = $('admin-cleanup-restore').disabled = state.busy;
  for (const button of document.querySelectorAll('[data-key]')) {
    button.disabled = !active;
  }
  $('text-input').disabled = !active;
  $('snapshot').disabled = !state.detailLive;
  $('rotate').disabled = !state.detailLive || !state.lease || state.busy;
  $('lease-info').hidden = !state.lease;
  $('open-adb').disabled = !state.lease || state.busy;
  if (!state.lease && $('adb-dialog').open) {
    clearHandoff();
    $('adb-status').hidden = false;
    $('adb-status').textContent = '브라우저의 조작 연결이 종료됐어요. 연결 명령을 실행했다면 Android Studio 기기 목록에서 이 기기를 계속 사용할 수 있어요.';
  }
  $('create-handoff').hidden = !state.lease;
  $('create-handoff').disabled = !state.lease || state.busy;
  if ($('adb-code').dataset.leaseId !== (state.lease?.id || '')) {
    clearHandoff();
    $('adb-code').dataset.leaseId = state.lease?.id || '';
  }
  if (state.lease) {
    $('lease-owner').textContent = state.lease.owner;
  }
  updateLeaseClock();
  $('admin-lease').hidden = !manageable;
  if (state.adminIntent && (!manageable || state.adminIntent.serial !== state.selected || state.adminIntent.leaseId !== device.leaseId)) state.adminIntent = null;
  if (manageable) {
    $('admin-owner').textContent = device.owner || '사용자';
    $('admin-owner').title = device.owner || '';
    $('admin-expiry').textContent = new Date(device.expiresAt).toLocaleTimeString('ko-KR', { hour: '2-digit', minute: '2-digit' });
    $('admin-takeover').disabled = state.busy || device.state !== 'device';
    $('admin-disconnect').disabled = state.busy;
  }
}

function ownLeaseExpiry() {
  const device = state.devices.find((item) => item.mine && item.occupied);
  const value = state.assignment?.lease?.expiresAt || device?.expiresAt;
  const time = value ? Date.parse(value) : NaN;
  return Number.isFinite(time) ? time : null;
}

function updateLeaseClock() {
  const until = ownLeaseExpiry();
  const left = until == null ? null : until - Date.now();
  const text = left == null ? '' : left <= 0 ? '만료됨' : countdown(left);
  const fraction = left == null ? 0 : Math.max(0, Math.min(1, left / 300000));
  const low = left != null && left < 60000;
  if (state.lease) {
    $('lease-expiry').textContent = until == null ? '—' : clock(new Date(until));
    $('lease-remaining').textContent = text ? `남은 시간 ${text}` : '';
    $('lease-meter').style.width = `${fraction * 100}%`;
    $('lease-info').classList.toggle('low', low);
  }
  updateAssignment(until, left, text, fraction, low);
}

function updateAssignment(until = ownLeaseExpiry(), left = until == null ? null : until - Date.now(), text = '', fraction = 0, low = false) {
  const strip = $('my-assignment');
  strip.hidden = !state.session?.authenticated || isAdmin();
  if (strip.hidden) return;
  const device = state.devices.find((item) => item.mine && item.occupied);
  const used = device ? 1 : 0;
  $('assignment-limit').textContent = `1대 중 ${used}대 사용 중`;
  $('assignment-body').hidden = !device;
  strip.classList.toggle('low', low && Boolean(device));
  $('assignment-help').textContent = device
    ? '이 브라우저가 열려 있는 동안 90초마다 점유가 5분씩 자동 연장돼요. 브라우저를 닫아 갱신이 멈추면 위 시각에 자동 반납됩니다.'
    : state.assigning ? '기기를 배정하고 있어요…' : '계정당 기기는 한 번에 1대만 사용할 수 있어요. 기기 배정받기를 누르면 사용 가능한 기기 1대를 연결합니다.';
  if (!device) return;
  $('assignment-name').textContent = modelName(device);
  $('assignment-serial').textContent = deviceLabel(device);
  $('assignment-until').textContent = until == null ? '만료 시각 확인 중' : `${clock(new Date(until))}까지`;
  $('assignment-remaining').textContent = text ? `남은 시간 ${text}` : '';
  $('assignment-meter').style.width = `${fraction * 100}%`;
  $('assignment-resume').disabled = state.busy || device.state !== 'device';
  $('assignment-release').disabled = state.busy || !state.assignment;
}

function endLease(message = '기기 점유가 종료됐어요', hint = '관리자가 연결을 끊었거나 점유 시간이 만료됐어요.') {
  if (!state.assignment && !state.lease) return;
  state.assignment = null;
  clearView();
  if (!state.lease) { updateDetail(); return; }
  state.lease = null;
  state.busy = false;
  state.detailEpoch++;
  detailAbort?.abort();
  detailAbort = new AbortController();
  detailPlayer?.close();
  detailPlayer = null;
  closeInput();
  resetTyped();
  placeholder(message, hint);
  updateDetail();
}

function openDevice(serial) {
  const device = state.devices.find((item) => item.serial === serial);
  if (!device || $('device-dialog').open) return;
  state.selected = serial;
  state.lease = state.assignment?.serial === serial ? state.assignment.lease : null;
  state.handoffSerial = state.tiles.get(serial)?.player ? serial : null;
  state.detailEpoch++;
  detailAbort = new AbortController();
  $('device-dialog').showModal();
  if (state.lease) rememberAssignment(serial, state.lease);
  for (const tile of state.tiles.values()) if (tile.player && tile.serial !== state.handoffSerial) stopTile(tile);
  updateDetail();
  syncDetail();
}

function syncDetail(previousState) {
  if (!state.selected || !$('device-dialog').open || state.busy) return;
  const device = state.devices.find((item) => item.serial === state.selected);
  if (!device || device.state !== 'device') {
    if (detailPlayer) {
      state.detailEpoch++;
      detailAbort?.abort(); detailAbort = new AbortController();
      detailPlayer.close(); detailPlayer = null;
      closeInput();
    }
    if (state.handoffSerial) {
      const tile = state.tiles.get(state.handoffSerial);
      if (tile) stopTile(tile);
      state.handoffSerial = null;
    }
    const hint = cleanupLabels[device?.state]?.hint || (device?.state === 'usb'
      ? 'USB는 감지됐지만 ADB 통신이 연결되지 않았어요. 연결이 복구되면 자동으로 다시 확인합니다.'
      : device?.state === 'unauthorized' ? '폰에 표시된 USB 디버깅 허용 창에서 이 컴퓨터를 승인해 주세요.'
        : '기기와 에이전트 연결을 기다리고 있어요. 연결이 복구되면 자동으로 다시 확인합니다.');
    placeholder(device ? stateLabel(device) : '오프라인', hint);
  } else if (previousState !== 'device') {
    if (isAdmin() || state.lease) connectDetail();
    else if (device.mine) placeholder('내게 배정된 기기예요', '조작 이어가기를 누르면 다시 연결합니다.');
    else if (device.occupied) placeholder('지금 사용 중인 기기예요', `${device.owner || '다른 사용자'} 님이 반납하면 연결할 수 있어요.`);
    else placeholder('테스트를 시작할 준비가 됐어요', '점유하고 조작을 누르면 기기 화면이 표시됩니다.');
  }
}

function connectDetail() {
  detailPlayer?.close();
  closeInput();
  if (!state.selected || document.hidden || (!state.lease && !isAdmin())) return;
  const epoch = state.detailEpoch;
  if (state.lease) {
    detailInput = new RealtimeInput({
      onState: (ready) => { if (epoch === state.detailEpoch) { if (!ready) { pointer = null; typedPending = false; } controls(); } },
      onUnauthorized: () => { if (epoch === state.detailEpoch) { if (!state.busy) endLease(); void refresh(); } },
      onError: (error) => { if (epoch === state.detailEpoch) notify(error.message); },
    });
    detailInput.start(`${devicePath()}/control`, state.lease.token);
  }
  let lastKeyframe = -Infinity;
  placeholder('기기 화면을 연결하고 있어요', state.lease ? '조작할 화면을 준비합니다.' : '읽기 전용 미리보기를 준비합니다.');
  detailPlayer = new VideoPlayer($('screen'), {
    onState: (status, message) => {
      if (epoch !== state.detailEpoch) return;
      if (status === 'configuring') placeholder('화면을 준비하고 있어요', '새 영상 프레임을 기다립니다.');
      else if (status === 'retrying' || status === 'error') placeholder(status === 'retrying' ? '기기 화면에 다시 연결하고 있어요' : '화면 연결을 확인해 주세요', message || '');
    },
    onFrame: ({ metadata, fps }) => {
      if (epoch !== state.detailEpoch) return;
      const wasLive = state.detailLive;
      if (state.handoffSerial) {
        const tile = state.tiles.get(state.handoffSerial);
        if (tile) stopTile(tile);
        state.handoffSerial = null;
      }
      state.detailLive = true;
      $('screen-placeholder').hidden = true;
      $('screen').hidden = false;
      $('live-badge').hidden = false;
      $('resolution').textContent = `${metadata.width} × ${metadata.height}`;
      $('frame-rate').textContent = `${fps || '—'} fps`;
      if (!wasLive) controls();
    },
    requestKeyframe: () => {
      if (performance.now() - lastKeyframe < 800) return;
      lastKeyframe = performance.now();
      if (state.lease) void action({ action: 'keyframe' }, false).catch(() => {});
      else if (isAdmin()) void api(`/api/admin/devices/${encodeURIComponent(state.selected)}/keyframe`, { method: 'POST' }).catch(() => {});
    },
    onUnauthorized: () => {
      if (epoch !== state.detailEpoch) return;
      if (state.lease && !state.busy) endLease();
      void refresh();
      void checkSession();
    },
  });
  detailPlayer.start(state.lease ? `${devicePath()}/video?token=${encodeURIComponent(state.lease.token)}` : previewPath(state.selected));
}

function closeInput() {
  pointer = null;
  typedPending = false;
  const input = detailInput; detailInput = null; input?.close();
}

async function assignDevice(selection) {
  if (state.assigning || state.assignment || state.devices.length) throw new Error('이미 배정된 기기가 있어요. 먼저 반납해 주세요.');
  state.assigning = true; renderDevices();
  const subject = state.session?.user?.subject;
  try {
    const assigned = await api('/api/device/occupy', { method: 'POST', body: { ttlSeconds: 300, ...selection }, timeout: 45000 });
    if (state.session?.user?.subject !== subject) {
      await api(`${devicePath(assigned.assigned)}/lease`, { method: 'DELETE', token: assigned.lease.token }).catch(() => {}); return;
    }
    state.devices = [assigned.device]; state.search = ''; state.filter = 'all'; filterPicker.setValue('all'); $('search').value = '';
    rememberAssignment(assigned.assigned, assigned.lease);
    renderDevices(); openDevice(assigned.assigned); updateDetail();
  } finally { state.assigning = false; renderDevices(); void refresh(); }
}
const assignChooser = setupAssign({ api, notify, onChoose: assignDevice });
$('assign-device').addEventListener('click', () => {
  if (state.assigning || state.assignment || state.devices.length) return;
  assignChooser.open();
});

async function action(body, log = true) {
  if (!state.lease || state.busy) throw new Error('먼저 기기를 점유해 주세요.');
  const token = state.lease.token, serial = state.selected;
  try {
    const result = realtimeActions.has(body.action)
      ? await detailInput.request(body)
      : await api(`${devicePath(serial)}/actions`, { method: 'POST', body, token, signal: detailAbort?.signal });
    return result;
  } catch (error) {
    if ([403, 410].includes(error.status) && state.lease?.token === token && state.selected === serial) { endLease(); void refresh(); }
    if (log && error.name !== 'AbortError') notify(error.message);
    throw error;
  }
}

async function acquireDevice({ leaseId } = {}) {
  if (state.busy || !state.selected) return;
  if (!('VideoDecoder' in globalThis)) return notify('최신 Chrome에서 HTTPS 또는 localhost로 접속해 주세요.');
  const serial = state.selected, epoch = state.detailEpoch;
  const workspaceEpoch = state.workspaceEpoch;
  const signal = detailAbort.signal;
  state.busy = true;
  placeholder('기기를 점유하고 있어요', '조작할 화면을 준비합니다.');
  let lease;
  try {
    const ownLeaseId = state.devices.find((device) => device.serial === serial && device.mine)?.leaseId;
    lease = await api(leaseId ? `/api/admin/devices/${encodeURIComponent(serial)}/lease` : `${devicePath(serial)}/lease`, { method: 'POST', body: { ttlSeconds: 300, ...(leaseId || ownLeaseId ? { leaseId: leaseId || ownLeaseId } : {}) }, signal });
    if (workspaceEpoch !== state.workspaceEpoch) return;
    rememberAssignment(serial, lease);
    if (epoch !== state.detailEpoch) return;
    await api(`${devicePath(serial)}/start`, { method: 'POST', token: lease.token, signal, timeout: 45000 });
    if (epoch !== state.detailEpoch) return;
    state.busy = false;
    connectDetail();
  } catch (error) {
    if (workspaceEpoch !== state.workspaceEpoch || epoch !== state.detailEpoch) return;
    state.lease = null;
    if (error.name !== 'AbortError') { placeholder('기기에 연결할 수 없어요', error.message); notify(error.message); }
  } finally {
    if (epoch === state.detailEpoch) { state.busy = false; updateDetail(); }
    void refresh();
  }
}
$('connect').addEventListener('click', () => { void acquireDevice(); });

function clearHandoff() {
  clearInterval(handoffTimer); handoffTimer = null;
  $('adb-code').hidden = true;
  $('adb-countdown').hidden = true;
  $('adb-command').textContent = '';
  $('adb-expiry').textContent = '';
}

function showHandoff(command, expiresAt) {
  clearHandoff();
  $('adb-command').textContent = command;
  const until = Date.parse(expiresAt) || Date.now() + 90000;
  const tick = () => {
    const left = Math.ceil((until - Date.now()) / 1000);
    if (left <= 0) { clearHandoff(); notify('연결 코드가 만료됐어요. 새 코드를 만들어 주세요.'); return; }
    $('adb-expiry').textContent = `${left}초`;
    $('adb-countdown').classList.toggle('urgent', left <= 15);
  };
  $('adb-code').hidden = false;
  $('adb-countdown').hidden = false;
  tick();
  handoffTimer = setInterval(tick, 1000);
}

$('open-adb').addEventListener('click', () => {
  if (!state.lease || $('adb-dialog').open) return;
  const device = state.devices.find((item) => item.serial === state.selected);
  $('adb-device').textContent = device ? `${modelName(device)} · ${deviceLabel(device)}` : '';
  $('adb-status').hidden = true;
  $('adb-dialog').showModal();
  controls();
});
function closeAdb() { clearHandoff(); if ($('adb-dialog').open) $('adb-dialog').close(); }
$('close-adb').addEventListener('click', closeAdb);
$('adb-dialog').addEventListener('close', clearHandoff);
$('adb-dialog').addEventListener('click', (event) => { if (event.target === $('adb-dialog') && event.clientX < $('adb-dialog').getBoundingClientRect().left) closeAdb(); });

$('create-handoff').addEventListener('click', async () => {
  if (!state.lease || state.busy) return;
  const lease = state.lease, serial = state.selected, epoch = state.detailEpoch;
  $('create-handoff').disabled = true;
  try {
    const result = await api(`${devicePath(serial)}/handoff`, { method: 'POST', token: lease.token, signal: detailAbort?.signal });
    if (epoch !== state.detailEpoch || state.lease?.token !== lease.token) return;
    const origin = new URL(apiUrl('/'), location.href).origin;
    const quote = (value) => `'${String(value).replaceAll("'", "'\\''")}'`;
    showHandoff(`export MARIONETTE_URL=${quote(origin)}\nmarionette attach --device ${quote(serial)} --code ${quote(result.code)} &&\n  marionette adb`, result.expiresAt);
  } catch (error) { if (epoch === state.detailEpoch && error.name !== 'AbortError') notify(error.message); }
  finally { if (epoch === state.detailEpoch) controls(); }
});
$('copy-adb').addEventListener('click', async () => {
  try { await navigator.clipboard.writeText($('adb-command').textContent); notify('연결 명령을 복사했어요.'); }
  catch { notify('명령을 선택해 직접 복사해 주세요.'); }
});

for (const [id, mode] of [['admin-takeover', 'takeover'], ['admin-disconnect', 'disconnect']]) $(id).addEventListener('click', () => {
  const device = state.devices.find((item) => item.serial === state.selected);
  if (!isAdmin() || state.busy || !device?.leaseId || state.lease) return;
  void adminConfirm({ mode, serial: device.serial, leaseId: device.leaseId, owner: device.owner });
});
async function adminConfirm(intent) {
  state.adminIntent = intent;
  const takeover = intent.mode === 'takeover';
  const ok = await confirmDialog({
    title: takeover ? '이 기기를 가져올까요?' : '사용자 연결을 끊을까요?',
    message: `${intent.owner || '현재 사용자'} 님의 화면 연결과 조작 권한이 바로 종료돼요.${takeover ? ' 이 기기는 내가 이어서 사용해요.' : ' 기기는 사용 가능 상태가 돼요.'}`,
    confirmLabel: takeover ? '가져오기' : '연결 끊기', danger: !takeover,
  });
  const current = state.adminIntent;
  state.adminIntent = null;
  if (!ok || !current || current.serial !== state.selected || current.leaseId !== intent.leaseId || state.busy || !isAdmin()) { controls(); return; }
  await runAdminIntent(current);
}
async function runAdminIntent(intent) {
  if (intent.mode === 'takeover') { await acquireDevice({ leaseId: intent.leaseId }); return; }
  const epoch = state.detailEpoch;
  state.busy = true; controls();
  try {
    await api(`/api/admin/devices/${encodeURIComponent(intent.serial)}/lease`, { method: 'DELETE', body: { leaseId: intent.leaseId }, signal: detailAbort?.signal });
    if (epoch !== state.detailEpoch) return;
    notify('사용자 연결을 끊고 기기를 반납시켰어요.');
  } catch (error) {
    if (epoch === state.detailEpoch && error.name !== 'AbortError') notify(error.status === 409 ? '점유자가 바뀌었어요. 새로 확인하고 다시 시도해 주세요.' : error.message);
  } finally {
    if (epoch === state.detailEpoch) { state.busy = false; updateDetail(); }
    void refresh();
  }
}

const wipeMessage = '설치한 앱과 내부 저장소(/sdcard)의 파일을 지워요. 기기 계정 로그인 같은 시스템 설정은 남을 수 있어요.';
const cleaningToast = '기기를 정리하는 중이에요. 끝나면 다른 사람이 쓸 수 있어요.';

for (const [id, operation] of [['admin-cleanup-retry', 'cleanup'], ['admin-cleanup-restore', 'restore']]) $(id).addEventListener('click', async () => {
  const serial = state.selected, epoch = state.detailEpoch;
  const current = () => epoch === state.detailEpoch && isAdmin() && state.devices.some((device) => device.serial === serial && device.state === 'cleaning_failed');
  if (state.busy || !current()) return;
  const retry = operation === 'cleanup';
  const confirmed = await confirmDialog({
    title: retry ? '기기를 다시 정리할까요?' : '정리 없이 기기를 복귀할까요?',
    message: retry ? wipeMessage : '기기에 남은 앱이나 파일이 다음 사용자에게 보일 수 있어요.',
    confirmLabel: retry ? '다시 정리' : '정리 없이 복귀', danger: true,
  });
  if (!confirmed || state.busy || !current()) return;
  state.busy = true; controls();
  try {
    await api(`/api/admin/devices/${encodeURIComponent(serial)}/${operation}`, { method: 'POST' });
    notify(retry ? cleaningToast : '기기를 복귀시켰어요.');
  } catch (error) { notify(error.message); }
  finally { if (epoch === state.detailEpoch) { state.busy = false; controls(); } void refresh(); }
});

async function returnDevice(fromDetail) {
  const assignment = fromDetail ? { serial: state.selected, lease: state.lease } : state.assignment;
  if (!assignment?.lease || state.busy || state.choosingRelease) return;
  const { serial, lease: { token } } = assignment;
  const epoch = state.detailEpoch, workspace = state.workspaceEpoch;
  const stillHere = () => workspace === state.workspaceEpoch && epoch === state.detailEpoch;
  state.choosingRelease = true;
  let choice;
  try {
    choice = await choiceDialog({ title: '기기를 어떻게 반납할까요?', message: wipeMessage, choices: [
      { value: 'plain', label: '그대로 반납' },
      { value: 'wipe', label: '정리하고 반납', danger: true },
    ] });
  } finally { state.choosingRelease = false; }
  const currentToken = fromDetail ? state.lease?.token : state.assignment?.lease.token;
  if (!choice || !stillHere() || state.busy || currentToken !== token) return;
  state.busy = true; controls();
  try {
    const result = await api(`${devicePath(serial)}/lease`, { method: 'DELETE', token, body: choice === 'wipe' ? { wipe: true } : undefined });
    if (state.assignment?.lease.token === token) { state.assignment = null; clearView(); }
    if (!stillHere()) return;
    if (fromDetail) {
      if (isAdmin() && !result.cleaning) { state.lease = null; connectDetail(); }
      else closeDetail();
    }
    notify(result.cleaning ? cleaningToast : '기기를 반납했어요.');
  } catch (error) { if (stillHere()) notify(`반납을 확인하지 못했어요. ${error.message}`); }
  finally { if (stillHere()) { state.busy = false; controls(); } void refresh(); }
}

$('release').addEventListener('click', () => { void returnDevice(true); });

$('assignment-resume').addEventListener('click', () => {
  const device = state.devices.find((item) => item.mine && item.occupied);
  if (!device || state.busy || $('device-dialog').open) return;
  openDevice(device.serial);
  if (!state.lease) void acquireDevice();
});
$('assignment-release').addEventListener('click', () => { if (!$('device-dialog').open) void returnDevice(false); });

function closeDetail() {
  appTools.sync('', false);
  fileTools.sync('', false);
  clearView();
  state.detailEpoch++;
  detailAbort?.abort(); detailPlayer?.close(); detailPlayer = null;
  closeInput();
  state.lease = null; state.selected = null; state.handoffSerial = null; state.busy = false; state.adminIntent = null; state.detailLive = false; pointer = null;
  if ($('device-dialog').open) $('device-dialog').close();
  queuePreviews();
}
$('close-device').addEventListener('click', closeDetail);
$('device-dialog').addEventListener('cancel', (event) => { event.preventDefault(); closeDetail(); });
$('device-dialog').addEventListener('click', (event) => { if (event.target === $('device-dialog') && event.clientX < $('device-dialog').getBoundingClientRect().left) closeDetail(); });

for (const button of document.querySelectorAll('[data-key]')) button.addEventListener('click', () => { void action({ action: 'key', keyCode: Number(button.dataset.key) }).catch(() => {}); });
// Typing goes through a hidden textarea so IME composition (Korean) works.
// The device replaces the focused field with the full value, so we mirror the buffer.
let typed = '', typing = false, typedPending = false;
async function flushTyped() {
  if (!detailInput?.ready) { typedPending = false; return; }
  if (typing) { typedPending = true; return; }
  typing = true;
  try { do { typedPending = false; if (state.lease && !state.busy) await action({ action: 'text', text: typed }, false); } while (typedPending); }
  catch { notify('이 입력란에는 입력할 수 없어요. 다른 입력란을 눌러 보세요.'); }
  finally { typing = false; }
}
const typer = $('text-input');
const resetTyped = () => { typed = typer.value = ''; };
typer.addEventListener('input', (event) => {
  if (event.isComposing) return;
  typed = typer.value; void flushTyped();
});
typer.addEventListener('compositionend', () => { typed = typer.value; void flushTyped(); });
typer.addEventListener('keydown', (event) => {
  if (event.isComposing || event.metaKey || event.ctrlKey || event.altKey) return;
  const code = { Enter: 66, Tab: 61, Escape: 4, ArrowUp: 19, ArrowDown: 20, ArrowLeft: 21, ArrowRight: 22 }[event.key];
  if (code === undefined && !(event.key === 'Backspace' && !typer.value)) return;
  event.preventDefault();
  if (event.key === 'Enter') resetTyped();
  void action({ action: 'key', keyCode: event.key === 'Backspace' ? 67 : code }, false).catch(() => {});
});
typer.addEventListener('paste', () => setTimeout(() => { typed = typer.value; void flushTyped(); }));
$('screen').addEventListener('pointerdown', (event) => {
  if (!state.lease || !state.detailLive || !detailInput?.ready || !detailPlayer?.metadata || event.button !== 0 || state.busy || pointer) return;
  event.preventDefault();
  pointer = { ...clientToDevice(event.clientX, event.clientY, $('screen').getBoundingClientRect(), detailPlayer.metadata), time: performance.now(), id: event.pointerId };
  $('screen').setPointerCapture(event.pointerId);
  void action({ action: 'touch', phase: 'down', x: pointer.x, y: pointer.y }, false).catch(() => { pointer = null; });
});
$('screen').addEventListener('pointermove', (event) => {
  if (!pointer || pointer.id !== event.pointerId || !detailPlayer?.metadata) return;
  const point = clientToDevice(event.clientX, event.clientY, $('screen').getBoundingClientRect(), detailPlayer.metadata);
  pointer.last = point;
  detailInput?.move({ action: 'touch', phase: 'move', ...point });
});
$('screen').addEventListener('pointerup', (event) => {
  if (!pointer || pointer.id !== event.pointerId || !detailPlayer?.metadata) return;
  const start = pointer; pointer = null;
  const end = clientToDevice(event.clientX, event.clientY, $('screen').getBoundingClientRect(), detailPlayer.metadata);
  const duration = performance.now() - start.time;
  resetTyped(); typer.focus({ preventScroll: true });
  void action({ action: 'touch', phase: 'up', ...end }, false).catch(() => {});
});
function cancelPointer() {
  const active = pointer; pointer = null;
  if (active && detailInput?.ready) void detailInput.request({ action: 'touch', phase: 'cancel', ...(active.last || {x:active.x,y:active.y}) }).catch(() => {});
}
$('screen').addEventListener('pointercancel', cancelPointer);
$('screen').addEventListener('lostpointercapture', cancelPointer);
window.addEventListener('blur', cancelPointer);
$('rotate').addEventListener('click', () => {
  const metadata = detailPlayer?.metadata;
  if (!state.lease || !state.detailLive || !metadata) return;
  // Derive the target from what the device is showing now, so one button flips both ways.
  const landscape = metadata.displayWidth > metadata.displayHeight;
  $('rotate').disabled = true;
  void action({ action: 'rotate', rotation: landscape ? 0 : 1 }, false)
    .catch((error) => { if (error.name !== 'AbortError') notify(`화면을 돌리지 못했어요. ${error.message}`); })
    .finally(() => { $('rotate').disabled = !state.detailLive || !state.lease || state.busy; });
});
$('snapshot').addEventListener('click', () => {
  if (!state.detailLive) return;
  $('screen').toBlob((blob) => {
    if (!blob) return;
    const url = URL.createObjectURL(blob), link = document.createElement('a');
    link.href = url; link.download = `marionette-${new Date().toISOString().replaceAll(':', '-')}.png`; link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  });
});

$('search').addEventListener('input', (event) => { state.search = event.target.value.trim().toLocaleLowerCase(); renderDevices(); });
$('density').addEventListener('input', (event) => { document.documentElement.style.setProperty('--tile-size', `${event.target.value}px`); });
$('toggle-live').addEventListener('click', () => {
  state.paused = !state.paused;
  $('toggle-live').setAttribute('aria-pressed', String(!state.paused));
  $('toggle-live').querySelector('use').setAttribute('href', state.paused ? '#i-play' : '#i-pause');
  $('toggle-live').querySelector('span').textContent = state.paused ? '실시간 재개' : '일시정지';
  queuePreviews(); updateSummary();
});
$('refresh').addEventListener('click', () => {
  for (const tile of state.tiles.values()) if (tile.player?.stopped) stopTile(tile);
  void refresh();
});
$('logout').addEventListener('click', async () => {
  $('logout').disabled = true;
  try {
    if (state.assignment) await api(`${devicePath(state.assignment.serial)}/lease`, { method: 'DELETE', token: state.assignment.lease.token }).catch(() => {});
    clearView();
    await api('/auth/logout', { method: 'POST' });
    stopWorkspace(); state.session = null; await checkSession();
  } catch (error) { notify(error.message); }
  finally { $('logout').disabled = false; }
});
$('login-button').addEventListener('click', (event) => { if ($('login-button').getAttribute('aria-disabled') === 'true') event.preventDefault(); });
document.addEventListener('keydown', (event) => {
  if (event.key === '/' && !['INPUT', 'TEXTAREA'].includes(document.activeElement?.tagName) && state.session?.authenticated && !$('device-dialog').open) { event.preventDefault(); $('search').focus(); }
});
document.addEventListener('visibilitychange', () => {
  queuePreviews();
  if (document.hidden) { for (const tile of state.tiles.values()) if (tile.player) stopTile(tile); detailPlayer?.close(); closeInput(); }
  else { if ($('device-dialog').open) connectDetail(); void checkSession(); void refresh(); }
});
setInterval(() => { if (!document.hidden) void refresh(); }, 5000);
setInterval(updateLeaseClock, 1000);
setInterval(() => { if (!document.hidden) void checkSession(); }, 60000);
setInterval(async () => {
  if (!state.assignment || state.busy) return;
  const { serial, lease: currentLease } = state.assignment;
  const token = currentLease.token;
  try {
    const lease = await api(`${devicePath(serial)}/lease`, { method: 'PUT', body: { ttlSeconds: 300 }, token });
    if (state.assignment?.lease.token === token) { rememberAssignment(serial, { ...currentLease, ...lease }); controls(); }
  } catch (error) {
    if (state.assignment?.lease.token !== token) return;
    if ([401, 403, 404, 409, 410].includes(error.status) || new Date(state.assignment.lease.expiresAt) <= new Date()) {
      endLease(); void refresh();
    }
    notify(`점유를 갱신하지 못했어요. ${error.message}`);
  }
}, 90000);
window.addEventListener('pagehide', () => {
  closeInput();
  for (const tile of state.tiles.values()) stopTile(tile);
  detailPlayer?.close();
});
void checkSession();
