import { apiUrl } from './config.mjs';
import { createPicker } from './picker.mjs';

const $ = (id) => document.getElementById(id);
const modes = {
  physical: ['실물 전용', 'USB로 꽂은 휴대폰만 운영해요. 가상 기기는 모두 꺼요.'],
  virtual: ['가상 전용', 'Android 가상 기기(에뮬레이터)만 만들어 운영해요.'],
  mixed: ['혼합', '실물 휴대폰과 가상 기기를 함께 운영해요.'],
};
const instanceStates = { ready: '준비됨', starting: '시작 중', restarting: '다시 시작 중' };
const quote = (value) => `'${String(value).replaceAll("'", "'\\''")}'`;

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}
function cell(label, main, sub) {
  const node = el('div', 'adm-cell'); node.dataset.label = label;
  node.append(el('span', '', main));
  if (sub) node.append(el('small', '', sub));
  return node;
}
const seen = (value) => (value && !String(value).startsWith('0001') ? new Date(value) : null);
function ago(date) {
  if (!date) return '';
  const seconds = Math.round((Date.now() - date.getTime()) / 1000);
  if (seconds < 45) return '방금 전';
  if (seconds < 3600) return `${Math.max(1, Math.round(seconds / 60))}분 전`;
  if (seconds < 86400) return `${Math.round(seconds / 3600)}시간 전`;
  return date.toLocaleDateString('ko-KR', { year: 'numeric', month: 'long', day: 'numeric' });
}
const stamp = (date) => (date ? date.toLocaleString('ko-KR', { dateStyle: 'medium', timeStyle: 'short' }) : '기록 없음');
function bytes(value) {
  const units = ['B', 'KB', 'MB', 'GB', 'TB']; let n = Number(value) || 0, i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i += 1; }
  return `${n >= 100 || i === 0 ? Math.round(n) : n.toFixed(1)} ${units[i]}`;
}
function duration(seconds) {
  const days = Math.floor(seconds / 86400), hours = Math.floor(seconds % 86400 / 3600), minutes = Math.floor(seconds % 3600 / 60);
  return days ? `${days}일 ${hours}시간` : hours ? `${hours}시간 ${minutes}분` : `${Math.max(1, minutes)}분`;
}
function meter(label, percent, text, note) {
  const node = el('div', 'adm-meter'); const head = el('div', 'adm-meter-head');
  head.append(el('span', '', label), el('strong', '', text));
  const track = el('div', 'adm-meter-track'); const fill = el('span', percent >= 90 ? 'danger' : percent >= 75 ? 'warn' : ''); fill.style.width = `${Math.max(0, Math.min(100, percent))}%`;
  track.setAttribute('role', 'progressbar'); track.setAttribute('aria-label', label); track.setAttribute('aria-valuenow', String(Math.round(percent))); track.setAttribute('aria-valuemin', '0'); track.setAttribute('aria-valuemax', '100');
  track.append(fill); node.append(head, track); if (note) node.append(el('small', '', note)); return node;
}
function unavailable(label, why) {
  const node = el('div', 'adm-meter is-off'); const head = el('div', 'adm-meter-head'); head.append(el('span', '', label), el('strong', '', '확인 불가'));
  node.append(head); if (why) node.append(el('small', '', why)); return node;
}
function meters(machine) {
  const m = machine?.metrics; if (!m) return [unavailable('CPU', '에이전트가 자원 정보를 아직 보내지 않았어요.')];
  const out = [];
  out.push(m.hasCpu ? meter('CPU', m.cpuPercent, `${Math.round(m.cpuPercent)}%`, `${machine.cores || '?'}코어${m.hasLoad ? ` · 부하 ${m.load1.toFixed(2)} / ${m.load5.toFixed(2)} / ${m.load15.toFixed(2)}` : ''}`) : unavailable('CPU', m.hasLoad ? `부하 ${m.load1.toFixed(2)} / ${m.load5.toFixed(2)} / ${m.load15.toFixed(2)}` : '이 PC에서는 읽지 못했어요.'));
  out.push(m.hasMemory ? meter('메모리', 100 * m.memoryUsed / m.memoryTotal, `${bytes(m.memoryUsed)} / ${bytes(m.memoryTotal)}`, `사용 가능 ${bytes(m.memoryTotal - m.memoryUsed)}`) : unavailable('메모리'));
  out.push(m.hasDisk ? meter('저장공간', 100 * m.diskUsed / m.diskTotal, `${bytes(m.diskUsed)} / ${bytes(m.diskTotal)}`, `에이전트 데이터가 있는 디스크 · 남은 공간 ${bytes(m.diskTotal - m.diskUsed)}`) : unavailable('저장공간'));
  if (m.hasUptime) { const up = el('div', 'adm-meter is-plain'); const head = el('div', 'adm-meter-head'); head.append(el('span', '', '가동 시간'), el('strong', '', duration(m.uptimeSec))); up.append(head); out.push(up); }
  return out;
}
const kindOf = (device) => (device.kind === 'virtual' ? 'virtual' : 'physical');
function deviceState(device) {
  if (device.state === 'cleaning') return ['정리 중', 'warn'];
  if (device.state === 'cleaning_failed') return ['정리 실패', 'danger'];
  if (device.state === 'disconnected') return ['연결 해제됨', ''];
  if (device.state === 'reconnecting') return ['재연결 중', 'warn'];
  if (device.debugRequested) return ['승인 대기', 'warn'];
  if (device.state === 'unauthorized') return ['인증 필요', 'warn'];
  if (device.state === 'usb') return ['디버깅 대기', 'warn'];
  if (device.state !== 'device') return ['오프라인', ''];
  if (device.leaseMode === 'maintenance') return ['점검 중', 'warn'];
  if (device.leaseMode === 'temporary') return ['임시 제어 중', 'warn'];
  return device.occupied ? ['사용 중', 'warn'] : ['사용 가능', 'ok'];
}
function configProblem(error) {
  const text = error.message || '';
  if (error.status === 409) return '이 에이전트의 기기가 아직 사용 중이에요. 사용 중인 기기를 모두 반납시킨 뒤 다시 저장해 주세요.';
  if (error.status === 503) return '에이전트가 오프라인이에요. PC에서 에이전트가 실행 중인지 확인한 뒤 다시 시도해 주세요.';
  if (error.status === 502) return '에이전트가 설정 요청에 응답하지 않았어요. 잠시 뒤 다시 시도해 주세요.';
  if (error.status === 403 || error.status === 401) return '관리자만 운영 설정을 바꿀 수 있어요. 다시 로그인해 주세요.';
  if (text.includes('install Android SDK')) return '이 PC에 Android SDK의 Emulator, Command-line Tools, 시스템 이미지가 없어서 가상 기기를 만들 수 없어요. 설치 후 다시 시도해 주세요.';
  if (text.includes('keepAwake requires')) return '“충전 중 화면 유지”는 실물 전용이나 혼합 모드에서만 쓸 수 있어요.';
  if (text.includes('physical mode requires')) return '실물 전용에서는 가상 기기를 0대로 두어야 해요.';
  if (text.includes('emulatorCount must be')) return '가상 기기 수가 이 PC에서 허용한 범위를 벗어났어요.';
  return `저장하지 못했어요. ${text}`.trim();
}

export function setupAgentDashboard({ api, notify, onOpenDevice }) {
  const rows = new Map();
  const connectionPending = new Set();
  let records = [], devices = [], unavailable = new Set(), devicesKnown = false;
  let active = false, admin = false, loading = false, epoch = 0, registering = false;
  let detailId = '', configId = '', registered = null, connectTimer = null, connectStarted = 0;

  // ---------- 화면 전환 (기기 모니터링 / 에이전트 / 사용자) ----------
  function go(name) {
    if (!admin) name = 'devices';
    active = name === 'agents';
    const labels = { devices: admin ? '기기 모니터링' : '모바일 기기', agents: '에이전트', users: '사용자' };
    $('devices-view').hidden = name !== 'devices';
    $('agents-view').hidden = name !== 'agents';
    $('users-view').hidden = name !== 'users';
    for (const [key, id] of [['devices', 'nav-devices'], ['agents', 'nav-agents'], ['users', 'nav-users']]) {
      const link = $(id);
      link.classList.toggle('selected', key === name);
      if (key === name) link.setAttribute('aria-current', 'page'); else link.removeAttribute('aria-current');
    }
    $('breadcrumb-label').textContent = labels[name];
    document.dispatchEvent(new CustomEvent('admin-view', { detail: name }));
    if (active) void refresh();
  }

  // ---------- 목록 ----------
  const hosted = (id) => devices.filter((device) => device.agentId === id);
  function counts(id) {
    const list = hosted(id);
    return { physical: list.filter((d) => kindOf(d) === 'physical').length, virtual: list.filter((d) => kindOf(d) === 'virtual').length, busy: list.filter((d) => d.occupied).length };
  }
  function modeText(record) {
    const machine = record.machine;
    if (!machine) return ['확인 전', record.connected ? '운영 정보를 받는 중이에요' : '에이전트가 연결되면 표시돼요'];
    const sub = machine.mode === 'physical' ? 'USB 휴대폰만' : `가상 기기 ${machine.emulatorCount || 0}대 유지`;
    return [modes[machine.mode]?.[0] || machine.mode, sub];
  }
  function canConfigure(record) { return Boolean(record.connected && record.machine && record.machine.configurable !== false); }
  function configureReason(record) {
    if (!record.connected) return '오프라인 에이전트는 설정할 수 없어요';
    if (!record.machine) return '에이전트가 운영 정보를 보내면 설정할 수 있어요';
    if (record.machine.configurable === false) return '이 에이전트는 운영 설정을 지원하지 않아요';
    return '';
  }
  function buildRow() {
    const row = el('div', 'adm-row is-clickable'); row.setAttribute('role', 'listitem'); row.tabIndex = 0;
    const main = el('div', 'adm-main'); main.append(el('strong'), el('small', 'mono'));
    const badge = el('span', 'badge'); badge.dataset.role = 'state';
    const action = el('div', 'adm-action'); const button = el('button', 'button compact', '운영 설정'); button.type = 'button'; action.append(button);
    row.append(main, badge, el('div'), el('div'), el('div'), el('div'), action);
    row.addEventListener('click', () => openDetail(row.dataset.id));
    row.addEventListener('keydown', (event) => { if (event.target === row && (event.key === 'Enter' || event.key === ' ')) { event.preventDefault(); openDetail(row.dataset.id); } });
    button.addEventListener('click', (event) => { event.stopPropagation(); openConfig(row.dataset.id); });
    return row;
  }
  function fillRow(row, record) {
    row.dataset.id = record.id;
    const [main, badge, , , , , action] = row.children;
    main.children[0].textContent = record.name || record.id; main.children[1].textContent = record.id;
    badge.textContent = record.connected ? '연결됨' : '오프라인'; badge.className = `badge${record.connected ? ' ok' : ''}`;
    const machine = record.machine, last = seen(record.lastSeen), count = counts(record.id);
    const os = machine?.os ? `${machine.os} · ${machine.arch}` : '정보 없음';
    const metrics = machine?.metrics;
    const usage = metrics && record.connected ? [metrics.hasCpu ? `CPU ${Math.round(metrics.cpuPercent)}%` : '', metrics.hasMemory ? `RAM ${Math.round(100 * metrics.memoryUsed / metrics.memoryTotal)}%` : '', metrics.hasDisk ? `디스크 ${Math.round(100 * metrics.diskUsed / metrics.diskTotal)}%` : ''].filter(Boolean).join(' · ') : '';
    const cpu = usage || (machine?.cores ? `CPU ${machine.cores}코어${machine.hostname ? ` · ${machine.hostname}` : ''}` : '');
    const [mode, modeSub] = modeText(record);
    const deviceMain = !devicesKnown ? '불러오는 중' : unavailable.has(record.id) || (!record.connected && !hosted(record.id).length) ? '확인 불가' : `실물 ${count.physical} · 가상 ${count.virtual}`;
    const deviceSub = devicesKnown && (count.physical + count.virtual) ? `사용 중 ${count.busy}대` : '';
    const lastMain = record.connected ? '지금 연결 중' : last ? ago(last) : '연결 기록 없음';
    const lastSub = !record.connected && last ? stamp(last) : '';
    row.children[2].replaceWith(cell('OS · CPU', os, cpu));
    row.children[3].replaceWith(cell('운영 방식', mode, modeSub));
    row.children[4].replaceWith(cell('기기', deviceMain, deviceSub));
    row.children[5].replaceWith(cell('마지막 연결', lastMain, lastSub));
    const button = action.firstElementChild;
    button.disabled = !canConfigure(record); button.title = configureReason(record);
    button.setAttribute('aria-label', `${record.name || record.id} 운영 설정`);
    row.setAttribute('aria-label', `${record.name || record.id} 상세 보기`);
  }
  function stat(label, value, tone) {
    const node = el('span', `adm-stat${tone ? ` ${tone}` : ''}`); node.append(el('span', '', label), el('strong', '', String(value))); return node;
  }
  function renderList() {
    const online = records.filter((r) => r.connected).length;
    const all = devices.filter((d) => records.some((r) => r.id === d.agentId));
    $('agent-count').textContent = records.length;
    $('agent-summary').replaceChildren(
      stat('연결됨', online, 'ok'), stat('오프라인', records.length - online), stat('실물', all.filter((d) => kindOf(d) === 'physical').length),
      stat('가상', all.filter((d) => kindOf(d) === 'virtual').length), stat('사용 중', all.filter((d) => d.occupied).length, 'warn'));
    $('agent-summary').hidden = !records.length;
    $('agent-table').hidden = !records.length; $('agents-empty').hidden = Boolean(records.length);
    const wanted = new Set(records.map((r) => r.id));
    for (const [id, row] of rows) if (!wanted.has(id)) { row.remove(); rows.delete(id); }
    for (const record of records) {
      let row = rows.get(record.id);
      if (!row) { row = buildRow(); rows.set(record.id, row); }
      fillRow(row, record);
      $('agent-list').append(row);
    }
    $('agents-status').textContent = records.length && !devicesKnown ? '기기 목록을 불러오지 못해 기기 수는 표시하지 못했어요.' : '';
  }

  async function refresh() {
    if (!admin || loading || (!active && !$('agent-detail-dialog').open)) return;
    loading = true;
    const version = epoch;
    try {
      const [agents, list] = await Promise.allSettled([api('/api/agents'), api('/api/devices')]);
      if (version !== epoch || !admin) return;
      if (agents.status === 'rejected') throw agents.reason;
      records = agents.value.agents || [];
      devicesKnown = list.status === 'fulfilled';
      if (devicesKnown) { devices = list.value.devices || []; unavailable = new Set(list.value.unavailableAgents || []); }
      renderList();
      if ($('agent-detail-dialog').open) renderDetail();
    } catch (error) {
      if (version === epoch) $('agents-status').textContent = `에이전트 목록을 불러오지 못했어요. ${error.message}`;
    } finally { loading = false; }
  }

  // ---------- 상세 팝업 ----------
  function fact(label, value, wide) {
    const node = el('div', wide ? 'wide' : ''); node.append(el('dt', '', label), el('dd', '', value || '-')); return node;
  }
  function openDetail(id) {
    if (!records.some((r) => r.id === id)) return;
    connectionFeedback('');
    detailId = id; renderDetail();
    if (!$('agent-detail-dialog').open) $('agent-detail-dialog').showModal();
  }
  function connectionFeedback(message, error = false) {
    const node = $('agent-device-feedback');
    node.className = error ? 'adm-error' : 'helper';
    node.textContent = message;
    node.hidden = !message;
  }
  async function changeConnection(device, action) {
    const id = detailId;
    const version = epoch;
    const name = device.model || device.rawSerial || device.serial;
    connectionPending.add(device.serial);
    connectionFeedback(`${name} 연결 요청 처리 중…`);
    renderDetail();
    try {
      const { state } = await api(`/api/admin/devices/${encodeURIComponent(device.serial)}/connection`, { method: 'POST', body: { action } });
      if (version !== epoch || detailId !== id) return;
      const current = devices.find((item) => item.serial === device.serial);
      if (current) {
        if (state === 'debug_requested') current.debugRequested = true;
        else if (state !== 'debug_unavailable') {
          current.state = state;
          current.debugRequested = false;
        }
      }
      const messages = {
        debug_unavailable: 'ADB 연결이 없어요. 휴대폰에서 USB 디버깅을 켜고 USB 연결을 확인해 주세요.',
        debug_requested: '디버깅 요청을 보냈어요. 휴대폰 잠금을 풀고 USB 디버깅 승인 팝업을 확인해 주세요.',
        disconnected: 'Marionette에서 연결을 해제했어요. 다시 연결하면 기기를 사용할 수 있어요.',
      };
      const message = messages[state] || (action === 'reconnect' ? 'Marionette에서 다시 연결했어요.' : '이미 USB 디버깅이 승인된 기기예요.');
      connectionFeedback(`${name} · ${message}`);
    } catch (error) {
      if (version !== epoch || detailId !== id) return;
      const message = error.message.includes('in use or cleanup') ? '기기 사용 또는 정리가 끝난 뒤 다시 시도해 주세요.'
        : error.message.includes('ADB could not reconnect') ? '이 기기의 디버깅 요청을 보내지 못했어요. USB 연결을 확인한 뒤 다시 시도해 주세요.'
          : error.status === 503 ? '에이전트가 오프라인이에요.' : `요청을 처리하지 못했어요. ${error.message}`;
      connectionFeedback(`${name} · ${message}`, true);
    } finally {
      connectionPending.delete(device.serial);
      if (version === epoch && detailId === id) {
        renderDetail();
        void refresh();
      }
    }
  }
  function deviceActions(device, record) {
    const node = el('div', 'adm-device-actions');
    const available = record.connected && devicesKnown && !unavailable.has(record.id);
    function add(action, label, disabled, run) {
      const button = el('button', 'button compact', label);
      button.type = 'button';
      button.disabled = disabled;
      const name = device.model || device.rawSerial || device.serial;
      button.setAttribute('aria-label', `${name} ${label}`);
      button.dataset.connectionAction = action;
      button.dataset.serial = device.serial;
      button.addEventListener('click', run);
      node.append(button);
    }
    if (device.state === 'device') add('open', '제어 열기', !available, () => {
      $('agent-detail-dialog').close();
      onOpenDevice(device);
    });
    if (kindOf(device) !== 'physical') return node;
    if (!device.connectionManageable) {
      node.append(el('small', 'adm-muted', '연결 관리는 에이전트 업데이트 필요'));
      return node;
    }
    const disabled = !available || connectionPending.has(device.serial) || device.occupied ||
      ['reconnecting', 'cleaning', 'cleaning_failed'].includes(device.state);
    const connect = (action, label) => add(action, label, disabled, () => void changeConnection(device, action));
    if (device.state === 'disconnected') connect('reconnect', '다시 연결');
    else {
      if (device.state !== 'device') connect('debug', '디버깅 요청');
      connect('disconnect', '연결 해제');
    }
    return node;
  }
  function renderDetail() {
    const record = records.find((r) => r.id === detailId);
    if (!record) { if ($('agent-detail-dialog').open) $('agent-detail-dialog').close(); return; }
    const machine = record.machine, last = seen(record.lastSeen), created = seen(record.createdAt);
    $('agent-detail-title').textContent = record.name || record.id;
    $('agent-detail-sub').textContent = record.id;
    $('agent-detail-state').textContent = record.connected ? '연결됨' : '오프라인'; $('agent-detail-state').className = `badge${record.connected ? ' ok' : ''}`;
    const [mode, modeSub] = modeText(record);
    $('agent-detail-facts').replaceChildren(
      fact('운영 방식', machine ? `${mode} · ${modeSub}` : mode),
      fact('PC', machine?.hostname ? `${machine.hostname} (${machine.os} · ${machine.arch})` : machine?.os ? `${machine.os} · ${machine.arch}` : ''),
      fact('CPU', machine?.cores ? `${machine.cores}코어` : ''),
      fact('마지막 연결', record.connected ? '지금 연결 중' : last ? `${ago(last)} (${stamp(last)})` : '연결 기록 없음'),
      fact('컨트롤러 준비', machine ? `${machine.controllersReady || 0}대 · 화면 보는 사용자 ${machine.viewers || 0}명` : ''),
      fact('등록일', created ? stamp(created) : ''),
    );
    $('agent-detail-meters').replaceChildren(...meters(machine));
    $('agent-detail-sampled').textContent = machine?.metrics?.sampledAt ? `${ago(seen(machine.metrics.sampledAt))} 측정` : '';
    $('agent-detail-error').textContent = machine?.error ? `에이전트가 보고한 문제: ${machine.error}` : ''; $('agent-detail-error').hidden = !machine?.error;
    const list = hosted(record.id);
    $('agent-detail-count').textContent = devicesKnown ? `${list.length}대` : '';
    const note = [];
    if (!devicesKnown) note.push('기기 목록을 불러오지 못했어요.');
    else if (unavailable.has(record.id) || !record.connected) note.push('이 에이전트가 오프라인이라 기기 목록을 확인할 수 없어요. 다시 연결되면 표시돼요.');
    else if (!list.length) note.push('이 에이전트에 연결된 기기가 없어요. 휴대폰을 USB로 꽂고 USB 디버깅 인증을 허용해 보세요.');
    const starting = (machine?.instances || []).filter((i) => !list.some((d) => d.rawSerial === i.serial));
    if (starting.length) note.push(`가상 기기 준비 중: ${starting.map((i) => `${i.serial} (${instanceStates[i.state] || i.state})`).join(', ')}`);
    $('agent-detail-note').textContent = note.join(' '); $('agent-detail-note').hidden = !note.length;
    $('agent-detail-table').hidden = !list.length;
    const focused = document.activeElement?.dataset;
    const deviceList = $('agent-detail-devices');
    deviceList.replaceChildren(...list.map((device) => {
      const row = el('div', 'adm-row'); row.setAttribute('role', 'listitem');
      const main = el('div', 'adm-main'); main.append(el('strong', '', device.model || device.rawSerial || device.serial), el('small', 'mono', device.rawSerial || device.serial));
      const [label, tone] = deviceState(device);
      const state = el('span', `badge${tone ? ` ${tone}` : ''}`, label); state.dataset.role = 'state';
      row.append(main, cell('종류', kindOf(device) === 'virtual' ? '가상' : '실물'), state, cell('사용자', device.occupied ? device.owner || '사용 중' : '사용 안 함'), deviceActions(device, record));
      return row;
    }));
    if (focused?.connectionAction) {
      [...deviceList.querySelectorAll('button')].find((button) =>
        button.dataset.serial === focused.serial && button.dataset.connectionAction === focused.connectionAction)?.focus({ preventScroll: true });
    }
    const button = $('agent-detail-config');
    button.disabled = !canConfigure(record); button.title = configureReason(record);
  }

  // ---------- 운영 설정 팝업 ----------
  const countPicker = createPicker({ label: '유지할 가상 기기 수', options: [{ value: 0, label: '0대' }], value: 0, onChange: () => syncConfig() });
  const imagePicker = createPicker({ label: 'Android 이미지', options: [], value: '', placeholder: '설치된 이미지 없음', onChange: () => syncConfig() });
  $('agent-config-count').append(countPicker.element); $('agent-config-image').append(imagePicker.element);
  const configMode = () => $('agent-config-modes').querySelector('input:checked')?.value || 'physical';
  function configError(message) { $('agent-config-error').textContent = message || ''; $('agent-config-error').hidden = !message; }
  function openConfig(id) {
    const record = records.find((r) => r.id === id);
    if (!record || !canConfigure(record)) return;
    const machine = record.machine, max = machine.maxEmulators || 0, available = Boolean(machine.emulatorAvailable);
    configId = id; configError('');
    $('agent-config-sub').textContent = `${record.name || record.id} · ${record.id}`;
    $('agent-config-modes').replaceChildren(...Object.entries(modes).map(([value, [title, description]]) => {
      const label = el('label', 'adm-radio-card'), radio = document.createElement('input');
      radio.type = 'radio'; radio.name = 'agent-mode'; radio.value = value; radio.checked = value === machine.mode;
      radio.disabled = value === 'virtual' && !available;
      radio.addEventListener('change', () => {
        if (value !== 'physical' && Number(countPicker.value) === 0 && available) countPicker.setValue(1);
        syncConfig();
      });
      label.append(radio, el('strong', '', title), el('small', '', value === 'virtual' && !available ? '이 PC에 가상 기기 도구가 없어서 쓸 수 없어요.' : description));
      return label;
    }));
    countPicker.setOptions(Array.from({ length: max + 1 }, (_, n) => ({ value: n, label: `${n}대` })));
    countPicker.setValue(Math.min(machine.emulatorCount || 0, max));
    const templates = machine.templates || [];
    imagePicker.setOptions(templates.map((t) => ({ value: t.id, label: t.name || t.id, description: t.name && t.name !== t.id ? t.id : '' })));
    imagePicker.setValue(machine.image || templates[0]?.id || '');
    $('agent-config-awake').checked = Boolean(machine.keepAwake); $('agent-config-warm').checked = machine.preWarm !== false;
    $('agent-config-hint').textContent = available
      ? `가상 기기는 한 대당 메모리 약 1.5GB와 CPU 2코어를 써요. 이 PC에서는 최대 ${max}대까지 만들 수 있어요. 설정을 바꾸려면 이 에이전트의 기기를 모두 반납한 상태여야 해요.`
      : '이 PC에는 가상 기기를 만드는 데 필요한 Android SDK(Emulator, Command-line Tools, 시스템 이미지)가 없어요. 실물 기기만 운영할 수 있어요. 설정을 바꾸려면 이 에이전트의 기기를 모두 반납한 상태여야 해요.';
    $('save-agent-config').disabled = false; $('save-agent-config').textContent = '저장';
    syncConfig();
    if (!$('agent-config-dialog').open) $('agent-config-dialog').showModal();
  }
  function syncConfig() {
    const record = records.find((r) => r.id === configId), machine = record?.machine;
    if (!machine) return;
    const mode = configMode(), available = Boolean(machine.emulatorAvailable);
    const noVirtual = mode === 'physical' || !available;
    $('agent-config-emulator').hidden = mode === 'physical';
    if (mode === 'physical') countPicker.setValue(0);
    countPicker.setDisabled(noVirtual);
    imagePicker.setDisabled(noVirtual || Number(countPicker.value) === 0 || !(machine.templates || []).length);
    const awake = $('agent-config-awake');
    if (mode === 'virtual') awake.checked = false;
    awake.disabled = mode === 'virtual';
  }
  $('agent-config-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    const record = records.find((r) => r.id === configId), button = $('save-agent-config');
    if (!record || button.disabled) return;
    const mode = configMode(), count = mode === 'physical' ? 0 : Number(countPicker.value);
    if (count > 0 && !imagePicker.value) { configError('가상 기기에 쓸 Android 이미지를 선택해 주세요.'); return; }
    if (mode === 'virtual' && count === 0) { configError('가상 전용에서는 가상 기기를 1대 이상 유지해야 해요.'); return; }
    const policy = { mode, emulatorCount: count, image: count > 0 ? imagePicker.value : record.machine.image || '', keepAwake: mode !== 'virtual' && $('agent-config-awake').checked, preWarm: $('agent-config-warm').checked };
    button.disabled = true; button.textContent = '저장 중…'; configError('');
    try {
      await api(`/api/admin/agents/${encodeURIComponent(record.id)}/config`, { method: 'PUT', body: policy, timeout: 60000 });
      notify('운영 설정을 저장했어요.');
      $('agent-config-dialog').close(); void refresh();
    } catch (error) { configError(configProblem(error)); button.disabled = false; button.textContent = '저장'; }
  });
  $('close-agent-config').addEventListener('click', () => $('agent-config-dialog').close());
  $('cancel-agent-config').addEventListener('click', () => $('agent-config-dialog').close());
  $('agent-config-dialog').addEventListener('close', () => { configId = ''; });

  $('close-agent-detail').addEventListener('click', () => $('agent-detail-dialog').close());
  $('agent-detail-close').addEventListener('click', () => $('agent-detail-dialog').close());
  $('agent-detail-dialog').addEventListener('close', () => { detailId = ''; });
  $('agent-detail-config').addEventListener('click', () => { const id = detailId; $('agent-detail-dialog').close(); openConfig(id); });

  // ---------- 에이전트 연결 팝업 ----------
  const installTabs = [...$('agent-install-tabs').querySelectorAll('[role="tab"]')];
  const emulatorSwitch = $('agent-install-emulator');
  let installDetails = null;
  function renderUbuntuCommand() {
    if (!installDetails) return;
    const { origin, env } = installDetails;
    const script = quote(`${origin}/install/ubuntu.sh`);
    const mode = emulatorSwitch.checked ? ' -- --mode emulator' : '';
    $('agent-ubuntu-command').textContent = `marionette_setup=$(curl -fsSL ${script}) && ${env.join(' ')} bash -c "$marionette_setup"${mode}`;
  }
  function showInstallTab(selected) {
    for (const tab of installTabs) {
      const active = tab === selected;
      tab.setAttribute('aria-selected', String(active));
      tab.tabIndex = active ? 0 : -1;
      $(tab.getAttribute('aria-controls')).hidden = !active;
    }
  }
  function setStep(step) {
    for (const n of [1, 2, 3]) {
      const node = $(`connect-step-${n}`);
      const current = step === 'wait' ? n >= 2 && n <= 3 : n === step, done = step === 'wait' ? n < 2 : n < step;
      node.classList.toggle('is-done', done); node.classList.toggle('is-current', current); node.classList.toggle('is-pending', !done && !current);
    }
  }
  function waitText(text, mode) { const node = $('agent-connect-wait'); node.textContent = text; node.className = `adm-wait${mode ? ` is-${mode}` : ''}`; }
  function stopPolling() { clearInterval(connectTimer); connectTimer = null; }
  function clearRegistration() {
    epoch++; registering = false; registered = null; stopPolling();
    installDetails = null; emulatorSwitch.checked = false; showInstallTab(installTabs[0]);
    $('agent-register-form').reset();
    $('agent-install-pending').hidden = false; $('new-agent-secret').hidden = true;
    $('agent-connect-command').textContent = ''; $('agent-ubuntu-command').textContent = '';
    $('agent-connect-status').textContent = ''; $('agent-connect-status').hidden = true; $('agent-connect-help').hidden = true;
    $('open-connected-config').hidden = true; $('done-agent-connect').textContent = '닫기';
    for (const field of $('agent-register-form').elements) field.disabled = false;
    setStep(1); waitText('위 단계를 마치면 이 창에서 연결 여부를 확인해요.');
  }
  async function pollConnection() {
    if (!registered) return;
    const version = epoch, id = registered.id;
    try {
      const result = await api('/api/agents', { timeout: 8000 });
      if (version !== epoch || !registered) return;
      const found = (result.agents || []).find((r) => r.id === id);
      if (found?.connected) {
        stopPolling(); setStep(4);
        waitText(`연결됐어요! “${found.name || id}”이(가) 에이전트 목록에 추가됐어요.`, 'success');
        $('agent-connect-help').hidden = true; $('open-connected-config').hidden = false; $('done-agent-connect').textContent = '완료';
        void refresh();
      } else if (Date.now() - connectStarted > 90000) $('agent-connect-help').hidden = false;
    } catch { /* 일시적인 오류는 다음 확인에서 다시 시도해요 */ }
  }
  $('nav-agents').addEventListener('click', () => go('agents'));
  $('nav-users').addEventListener('click', () => go('users'));
  $('nav-devices').addEventListener('click', (event) => { event.preventDefault(); go('devices'); });
  const openConnect = () => { clearRegistration(); $('agent-connect-dialog').showModal(); $('new-agent-id').focus(); };
  $('add-agent').addEventListener('click', openConnect);
  $('add-agent-empty').addEventListener('click', openConnect);
  $('close-agent-connect').addEventListener('click', () => $('agent-connect-dialog').close());
  $('done-agent-connect').addEventListener('click', () => $('agent-connect-dialog').close());
  $('agent-connect-dialog').addEventListener('close', clearRegistration);
  for (const [index, tab] of installTabs.entries()) {
    tab.addEventListener('click', () => showInstallTab(tab));
    tab.addEventListener('keydown', (event) => {
      const next = event.key === 'Home' ? 0 : event.key === 'End' ? installTabs.length - 1
        : event.key === 'ArrowLeft' ? index - 1 : event.key === 'ArrowRight' ? index + 1 : null;
      if (next === null) return;
      event.preventDefault();
      const target = installTabs[(next + installTabs.length) % installTabs.length];
      showInstallTab(target); target.focus();
    });
  }
  emulatorSwitch.addEventListener('change', renderUbuntuCommand);
  $('open-connected-config').addEventListener('click', async () => {
    const id = registered?.id; $('agent-connect-dialog').close();
    if (!id) return;
    await refresh();
    if (records.some((r) => r.id === id)) openConfig(id);
  });
  $('agent-register-form').addEventListener('submit', async (event) => {
    event.preventDefault(); if (registering) return;
    const id = $('new-agent-id').value.trim(), name = $('new-agent-name').value.trim();
    if (!$('new-agent-id').checkValidity()) { $('agent-connect-status').textContent = '에이전트 ID는 영문·숫자로 시작하고, 영문·숫자·-·_ 만 64자까지 쓸 수 있어요.'; $('agent-connect-status').hidden = false; return; }
    registering = true; const version = epoch; $('register-agent').disabled = true; $('agent-connect-status').hidden = true;
    try {
      const result = await api('/api/admin/agents', { method: 'POST', body: { id, name } });
      if (version !== epoch) return;
      const origin = new URL(apiUrl('/'), location.origin).origin;
      const local = ['127.0.0.1', 'localhost', '[::1]'].includes(new URL(origin).hostname);
      const env = [`MARIONETTE_BACKEND_URL=${quote(origin)}`, `MARIONETTE_AGENT_ID=${quote(result.id)}`, `MARIONETTE_AGENT_TOKEN=${quote(result.token)}`];
      installDetails = { origin, env };
      renderUbuntuCommand();
      $('agent-connect-command').textContent = ['# 에이전트 패키지를 푼 폴더에서 실행하세요', ...env.map((value) => `export ${value}`), 'bash start-agent.sh'].join('\n');
      $('agent-connect-note').textContent = local
        ? '이 서버 주소는 현재 컴퓨터에서만 접속할 수 있어요. 다른 PC에서 실행하려면 명령에 있는 서버 주소를 모두 그 PC에서 접속할 수 있는 HTTPS 주소로 바꿔 주세요.'
        : '에이전트가 이 서버로 먼저 접속해요. PC에서 열어 둘 포트는 없어요. 외부 서버 주소는 HTTPS여야 해요.';
      $('agent-install-pending').hidden = true;
      $('new-agent-secret').hidden = false;
      for (const field of $('agent-register-form').elements) field.disabled = true;
      registered = { id: result.id, name }; connectStarted = Date.now();
      setStep('wait'); waitText('연결 대기 중… PC에서 명령을 실행하면 자동으로 감지해요.', 'waiting');
      stopPolling(); connectTimer = setInterval(() => { void pollConnection(); }, 2500);
      void refresh();
    } catch (error) {
      if (version === epoch) {
        $('agent-connect-status').textContent = error.status === 409 ? '이미 등록된 에이전트 ID예요. 다른 ID를 입력해 주세요.' : error.status === 503 ? '에이전트 등록을 저장할 수 없어요. 서버 설정을 확인해 주세요.' : error.message;
        $('agent-connect-status').hidden = false; $('register-agent').disabled = false;
      }
    } finally { registering = false; }
  });
  async function copy(text, done) { try { await navigator.clipboard.writeText(text); notify(done); } catch { notify('복사하지 못했어요. 명령을 직접 선택해서 복사해 주세요.'); } }
  $('copy-agent-ubuntu').addEventListener('click', () => copy($('agent-ubuntu-command').textContent, '설치 명령을 복사했어요.'));
  $('copy-agent-command').addEventListener('click', () => copy($('agent-connect-command').textContent, '연결 명령을 복사했어요.'));
  $('copy-agent-env').addEventListener('click', () => copy(installDetails?.env.join('\n') || '', '.env 내용을 복사했어요.'));

  setInterval(() => { void refresh(); }, 5000);
  return {
    setAdmin(value) {
      admin = value; $('nav-agents').hidden = !value; $('nav-users').hidden = !value;
      if (!value && (!$('agents-view').hidden || !$('users-view').hidden)) go('devices');
    },
    stop() {
      admin = false; epoch++; loading = false; go('devices');
      rows.clear(); records = []; devices = []; devicesKnown = false; unavailable = new Set();
      $('agent-list').replaceChildren(); $('agent-summary').replaceChildren();
      for (const id of ['agent-connect-dialog', 'agent-detail-dialog', 'agent-config-dialog']) $(id).close();
      clearRegistration();
    },
  };
}
