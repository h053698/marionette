import { confirmDialog } from './dialogs.mjs';

const $ = (id) => document.getElementById(id);

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
const parse = (value) => { const date = value ? new Date(value) : null; return date && !Number.isNaN(date.getTime()) ? date : null; };
function ago(date) {
  if (!date) return '';
  const seconds = Math.round((Date.now() - date.getTime()) / 1000);
  if (seconds < 45) return '방금 전';
  if (seconds < 3600) return `${Math.max(1, Math.round(seconds / 60))}분 전`;
  if (seconds < 86400) return `${Math.round(seconds / 3600)}시간 전`;
  return date.toLocaleDateString('ko-KR', { year: 'numeric', month: 'long', day: 'numeric' });
}
const stamp = (date) => (date ? date.toLocaleString('ko-KR', { dateStyle: 'medium', timeStyle: 'short' }) : '기록 없음');
function remaining(date) {
  if (!date) return '';
  const ms = date.getTime() - Date.now();
  if (ms <= 0) return '곧 반납돼요';
  const minutes = Math.floor(ms / 60000), seconds = Math.floor((ms % 60000) / 1000);
  return minutes ? `${minutes}분 ${String(seconds).padStart(2, '0')}초 남음` : `${seconds}초 남음`;
}
const isAdminUser = (user) => user.role === 'admin';
const displayName = (user) => user.name || user.email || user.subject;
function problem(error) {
  if (error.status === 404) return '사용자 목록 기능을 이 서버에서 아직 쓸 수 없어요. 백엔드를 최신 버전으로 다시 시작해야 해요.';
  if (error.status === 403) return '관리자만 사용자 목록을 볼 수 있어요.';
  if (error.status === 503) return '사용자 정보를 저장한 곳에 아직 연결되지 않았어요. 서버 설정을 확인해 주세요.';
  return `사용자 목록을 불러오지 못했어요. ${error.message}`;
}

export function setupUsers({ api, notify }) {
  const rows = new Map(), deviceMap = new Map();
  let users = [], active = false, admin = false, loading = false, epoch = 0, loaded = false, failed = '';
  let detailSubject = '', disconnecting = false, timer = null;

  function matches(user, query) {
    if (!query) return true;
    return [user.name, user.email, user.subject, user.device].some((value) => String(value || '').toLowerCase().includes(query));
  }
  function stat(label, value, tone) {
    const node = el('span', `adm-stat${tone ? ` ${tone}` : ''}`); node.append(el('span', '', label), el('strong', '', String(value))); return node;
  }
  function buildRow() {
    const row = el('div', 'adm-row is-clickable'); row.setAttribute('role', 'listitem'); row.tabIndex = 0;
    const main = el('div', 'adm-main'); main.append(el('strong'), el('small'));
    const role = el('span', 'badge'); role.dataset.label = '권한';
    row.append(main, role, el('div'), el('div'), el('div'), el('div'));
    row.addEventListener('click', () => openDetail(row.dataset.subject));
    row.addEventListener('keydown', (event) => { if (event.target === row && (event.key === 'Enter' || event.key === ' ')) { event.preventDefault(); openDetail(row.dataset.subject); } });
    return row;
  }
  function deviceText(user) {
    if (!user.device) return ['사용 안 함', ''];
    return [deviceLabel(user), remaining(parse(user.deviceLeaseExpiresAt))];
  }
  function deviceLabel(user) {
    const device = deviceMap.get(user.device);
    if (!device) return '기기 정보 확인 중';
    return `${device.model || device.rawSerial}${device.agentId ? ` · ${device.agentId}` : ''}`;
  }
  function fillRow(row, user) {
    row.dataset.subject = user.subject;
    const [main, role] = row.children;
    main.children[0].textContent = displayName(user);
    main.children[1].textContent = user.name && user.email ? user.email : '';
    main.children[1].hidden = !main.children[1].textContent;
    role.textContent = isAdminUser(user) ? '관리자' : '일반'; role.className = `badge${isAdminUser(user) ? ' ok' : ''}`;
    const login = parse(user.lastLoginAt), seen = parse(user.lastSeenAt);
    const online = el('div', 'adm-cell'); online.dataset.label = '접속';
    const status = el('span'); status.append(Object.assign(el('i', `adm-dot${user.online ? ' on' : ''}`), {}), document.createTextNode(user.online ? '접속 중' : '오프라인'));
    online.append(status);
    if (user.online && user.activeSessions > 1) online.append(el('small', '', `세션 ${user.activeSessions}개`));
    else if (!user.online && seen) online.append(el('small', '', `${ago(seen)} 확인`));
    const [device, left] = deviceText(user);
    row.children[2].replaceWith(online);
    row.children[3].replaceWith(cell('마지막 로그인', login ? ago(login) : '기록 없음', login ? stamp(login) : ''));
    row.children[4].replaceWith(cell('토큰', user.tokenCount ? `${user.tokenCount}개` : '없음'));
    row.children[5].replaceWith(cell('사용 중인 기기', device, left));
    row.setAttribute('aria-label', `${displayName(user)} 상세 보기`);
  }
  function render() {
    const query = $('user-search').value.trim().toLowerCase();
    const shown = users.filter((user) => matches(user, query));
    $('user-count').textContent = users.length;
    $('user-summary').replaceChildren(
      stat('전체', users.length), stat('접속 중', users.filter((u) => u.online).length, 'ok'),
      stat('관리자', users.filter(isAdminUser).length), stat('기기 사용 중', users.filter((u) => u.device).length, 'warn'));
    $('user-summary').hidden = !loaded || !users.length;
    const wanted = new Set(shown.map((u) => u.subject));
    for (const [subject, row] of rows) if (!wanted.has(subject)) { row.remove(); rows.delete(subject); }
    for (const user of shown) {
      let row = rows.get(user.subject);
      if (!row) { row = buildRow(); rows.set(user.subject, row); }
      fillRow(row, user); $('user-list').append(row);
    }
    let message = '';
    if (failed) message = failed;
    else if (loaded && !users.length) message = '아직 로그인한 사용자가 없어요. 사용자가 이 웹사이트에 처음 로그인하면 여기에 나타나요.';
    else if (loaded && !shown.length) message = '검색 결과가 없어요. 이름이나 이메일을 다시 확인해 보세요.';
    $('users-status').textContent = message;
    $('user-table').hidden = !shown.length;
  }

  async function refresh() {
    if (!admin || loading || (!active && !$('user-detail-dialog').open)) return;
    loading = true;
    const version = epoch;
    try {
      const [result, list] = await Promise.all([api('/api/admin/users'), api('/api/devices').catch(() => null)]);
      if (version !== epoch || !admin) return;
      users = Array.isArray(result.users) ? result.users : []; loaded = true; failed = '';
      if (list) { deviceMap.clear(); for (const device of list.devices || []) deviceMap.set(device.serial, device); }
    } catch (error) {
      if (version !== epoch) return;
      failed = problem(error); if (error.status === 404 || error.status === 403 || error.status === 503) { users = []; loaded = false; }
    } finally { loading = false; }
    if (version === epoch) { render(); if ($('user-detail-dialog').open) renderDetail(); }
  }

  // ---------- 상세 팝업 ----------
  function fact(label, value, wide) {
    const node = el('div', wide ? 'wide' : ''); node.append(el('dt', '', label), el('dd', '', value || '-')); return node;
  }
  function resetConfirm() {
    $('user-detail-disconnect').textContent = '이 사용자의 기기 연결 끊기'; $('user-detail-disconnect').disabled = false;
    $('user-detail-disconnect-note').textContent = '사용자의 화면 연결과 조작 권한이 바로 종료돼요.';
  }
  function openDetail(subject) {
    if (!users.some((u) => u.subject === subject)) return;
    detailSubject = subject; resetConfirm(); renderDetail();
    if (!$('user-detail-dialog').open) $('user-detail-dialog').showModal();
  }
  function renderDetail() {
    const user = users.find((u) => u.subject === detailSubject);
    if (!user) { if ($('user-detail-dialog').open) $('user-detail-dialog').close(); return; }
    const login = parse(user.lastLoginAt), seen = parse(user.lastSeenAt), expires = parse(user.deviceLeaseExpiresAt);
    $('user-detail-title').textContent = displayName(user);
    $('user-detail-sub').textContent = user.name && user.email ? user.email : '';
    $('user-detail-sub').hidden = !$('user-detail-sub').textContent;
    $('user-detail-role').textContent = isAdminUser(user) ? '관리자' : '일반'; $('user-detail-role').className = `badge${isAdminUser(user) ? ' ok' : ''}`;
    $('user-detail-facts').replaceChildren(
      fact('접속 상태', user.online ? `접속 중${user.activeSessions ? ` · 세션 ${user.activeSessions}개` : ''}` : '오프라인'),
      fact('API / MCP 토큰', `${user.tokenCount || 0}개`),
      fact('마지막 로그인', login ? `${ago(login)} (${stamp(login)})` : '기록 없음'),
      fact('마지막 활동', seen ? `${ago(seen)} (${stamp(seen)})` : '기록 없음'),
      fact('계정 ID', user.subject, true),
    );
    $('user-detail-device').hidden = false;
    $('user-detail-device-text').textContent = user.device ? `${deviceLabel(user)}${expires ? ` · ${remaining(expires)} (${stamp(expires)}까지)` : ''}` : '지금 사용 중인 기기가 없어요.';
    $('user-detail-disconnect-row').hidden = !user.device;
    if (!user.device) resetConfirm();
  }
  async function disconnect() {
    const user = users.find((u) => u.subject === detailSubject);
    if (!user?.device || disconnecting) return;
    const ok = await confirmDialog({
      title: `${displayName(user)}님의 연결을 끊을까요?`,
      message: `사용 중인 기기(${user.device})의 화면 연결과 조작 권한이 바로 종료되고 기기는 사용 가능 상태가 돼요.`,
      confirmLabel: '연결 끊기', danger: true,
    });
    if (!ok || !$('user-detail-dialog').open || detailSubject !== user.subject) return;
    disconnecting = true; $('user-detail-disconnect').disabled = true; $('user-detail-disconnect-note').textContent = '기기 연결을 끊고 있어요.';
    const version = epoch;
    try {
      const list = (await api('/api/devices')).devices || [];
      const device = list.find((item) => item.serial === user.device && item.occupied && item.leaseId);
      if (!device) { notify('이 사용자는 이미 기기를 반납했어요.'); void refresh(); return; }
      await api(`/api/admin/devices/${encodeURIComponent(device.serial)}/lease`, { method: 'DELETE', body: { leaseId: device.leaseId } });
      notify('사용자 연결을 끊고 기기를 반납시켰어요.');
    } catch (error) {
      $('user-detail-disconnect-note').textContent = error.status === 409 ? '점유자가 바뀌었어요. 새로 확인하고 다시 시도해 주세요.' : `연결을 끊지 못했어요. ${error.message}`;
      return;
    } finally { disconnecting = false; $('user-detail-disconnect').disabled = false; }
    if (version === epoch) { resetConfirm(); void refresh(); }
  }

  document.addEventListener('admin-view', (event) => {
    active = event.detail === 'users' && admin;
    if (active) { void refresh(); clearInterval(timer); timer = setInterval(() => { void refresh(); }, 10000); }
    else { clearInterval(timer); timer = null; }
  });
  $('user-search').addEventListener('input', render);
  $('refresh-users').addEventListener('click', () => { void refresh(); });
  $('close-user-detail').addEventListener('click', () => $('user-detail-dialog').close());
  $('user-detail-close').addEventListener('click', () => $('user-detail-dialog').close());
  $('user-detail-dialog').addEventListener('close', () => { detailSubject = ''; resetConfirm(); });
  $('user-detail-disconnect').addEventListener('click', () => { void disconnect(); });
  return {
    setAdmin(value) { admin = value; if (!value) { active = false; clearInterval(timer); timer = null; } },
    stop() {
      admin = false; active = false; epoch++; loading = false; clearInterval(timer); timer = null;
      users = []; loaded = false; failed = ''; rows.clear(); deviceMap.clear(); $('user-list').replaceChildren(); $('user-summary').replaceChildren(); $('user-search').value = '';
      $('user-detail-dialog').close();
    },
  };
}
