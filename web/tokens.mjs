import { apiUrl } from './config.mjs';
import { createPicker } from './picker.mjs';
import { confirmDialog } from './dialogs.mjs';

const PLACEHOLDER = '<발급한 토큰>';
const MAX_TOKENS = 20;

export function setupTokenSettings({ api, notify }) {
  const $ = (id) => document.getElementById(id);
  const dialog = $('token-dialog');
  let epoch = 0;
  let busy = false;
  let secret = '';
  let tool = 'mcp';

  const days = createPicker({
    label: '토큰 유효기간',
    value: 30,
    options: [
      { value: 7, label: '7일', description: '잠깐 시험해 볼 때' },
      { value: 30, label: '30일', description: '일반적인 사용' },
      { value: 90, label: '90일', description: '최대 기간' },
    ],
  });
  $('token-days-mount').append(days.element);
  days.element.querySelector('.picker-button').setAttribute('aria-labelledby', 'token-days-label');

  const origin = () => new URL(apiUrl('/'), location.href).origin;
  const snippets = (token) => {
    const url = origin();
    const env = `export MARIONETTE_URL=${url}\nexport MARIONETTE_TOKEN=${token}`;
    const auth = '-H "Authorization: Bearer $MARIONETTE_TOKEN"';
    return {
      'mcp-url': `${url}/mcp`,
      mcp: JSON.stringify({ mcpServers: { marionette: {
        command: '/absolute/path/to/marionette-mcp',
        env: { MARIONETTE_URL: url, MARIONETTE_TOKEN: token },
      } } }, null, 2),
      'cli-env': env,
      'cli-run': [
        'marionette devices                     # 내게 배정된 기기 (처음엔 비어 있어요)',
        'marionette occupy --platform android   # 기기 1대 배정받기',
        'marionette screenshot -o screen.png    # 화면 저장',
        'marionette release                     # 다 쓰면 반납',
      ].join('\n'),
      'rest-occupy': [
        env,
        '',
        '# 내게 배정된 기기 목록',
        `curl ${auth} $MARIONETTE_URL/api/devices`,
        '',
        '# 기기 1대 배정 (응답의 assigned = 기기 ID, lease.token = 조작용 토큰)',
        `curl -X POST $MARIONETTE_URL/api/device/occupy ${auth} -H "Content-Type: application/json" -d '{"platform":"android"}'`,
      ].join('\n'),
      'rest-act': [
        '# 화면 탭 (X-Lease-Token에 lease.token, deviceId에 assigned 값)',
        `curl -X POST $MARIONETTE_URL/api/actions/tap ${auth} -H "X-Lease-Token: <lease.token>" -H "Content-Type: application/json" -d '{"deviceId":"<assigned>","x":540,"y":1200}'`,
        '',
        '# 반납',
        `curl -X DELETE $MARIONETTE_URL/api/devices/<assigned>/lease ${auth} -H "X-Lease-Token: <lease.token>"`,
      ].join('\n'),
      'adb-env': env,
      'adb-run': [
        'marionette occupy --platform android   # 기기 배정',
        'marionette adb                         # adb 연결 (이 창은 켜 두세요)',
        '',
        '# 다른 터미널에서',
        'adb devices',
      ].join('\n'),
    };
  };
  const fillSnippets = () => {
    const values = snippets(secret || PLACEHOLDER);
    for (const code of dialog.querySelectorAll('[data-snippet]')) code.textContent = values[code.dataset.snippet];
  };

  const showTool = (name) => {
    tool = name;
    for (const tab of dialog.querySelectorAll('.tool-tabs [role="tab"]')) {
      const on = tab.dataset.tool === name;
      tab.setAttribute('aria-selected', String(on));
      tab.tabIndex = on ? 0 : -1;
    }
    for (const panel of dialog.querySelectorAll('[data-tool-panel]')) panel.hidden = panel.dataset.toolPanel !== name;
  };
  const setStep = (step) => {
    for (const item of dialog.querySelectorAll('#token-stepper li')) {
      const n = Number(item.dataset.step);
      item.classList.toggle('is-current', n === step);
      item.classList.toggle('is-done', n < step);
    }
  };
  const tabs = [...dialog.querySelectorAll('.tool-tabs [role="tab"]')];
  for (const tab of tabs) {
    tab.addEventListener('click', () => showTool(tab.dataset.tool));
    tab.addEventListener('keydown', (event) => {
      const at = tabs.indexOf(tab);
      const next = event.key === 'ArrowRight' ? at + 1 : event.key === 'ArrowLeft' ? at - 1 : event.key === 'Home' ? 0 : event.key === 'End' ? tabs.length - 1 : null;
      if (next === null) return;
      event.preventDefault();
      const target = tabs[(next + tabs.length) % tabs.length];
      target.focus(); showTool(target.dataset.tool);
    });
  }
  for (const item of dialog.querySelectorAll('#token-stepper li')) {
    item.querySelector('button').addEventListener('click', () => {
      setStep(Number(item.dataset.step));
      $(`token-step-${item.dataset.step}`).scrollIntoView({ block: 'start', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
    });
  }

  const clearSecret = () => {
    secret = '';
    $('token-secret').value = '';
    $('new-token').hidden = true;
    fillSnippets();
  };
  const reset = () => {
    epoch++;
    busy = false;
    $('create-token').disabled = false;
    $('token-form').reset();
    days.setValue(30);
    $('token-status').textContent = '';
    $('token-count').textContent = '';
    $('token-list').replaceChildren();
    $('grant-list').replaceChildren();
    $('grant-count').textContent = '';
    clearSecret();
    showTool('mcp');
    setStep(1);
  };

  const day = 86400000;
  const fmt = (value) => new Date(value).toLocaleDateString('ko-KR', { year: 'numeric', month: 'long', day: 'numeric' });
  const meta = (label, value) => {
    const item = document.createElement('span');
    const key = document.createElement('em'); key.textContent = label;
    item.append(key, ` ${value}`);
    return item;
  };

  const load = async () => {
    const current = epoch;
    $('token-status').textContent = '토큰을 확인하고 있어요.';
    try {
      const { tokens } = await api('/api/tokens');
      if (current !== epoch || !dialog.open) return;
      const list = $('token-list');
      list.replaceChildren();
      $('token-count').textContent = `${tokens.length}/${MAX_TOKENS}`;
      if (!tokens.length) {
        const empty = document.createElement('p');
        empty.className = 'helper';
        empty.textContent = '아직 만든 토큰이 없어요. 위 1단계에서 만들어 보세요.';
        list.append(empty);
      }
      for (const token of tokens) {
        const row = document.createElement('div'); row.className = 'token-row';
        const detail = document.createElement('div'); detail.className = 'token-detail';
        const name = document.createElement('strong'); name.textContent = token.name;
        const left = Math.ceil((new Date(token.expiresAt).getTime() - Date.now()) / day);
        const state = document.createElement('span');
        state.className = `badge token-state${left <= 0 ? ' expired' : left <= 7 ? ' soon' : ''}`;
        state.textContent = left <= 0 ? '만료됨' : `${left}일 남음`;
        const title = document.createElement('div'); title.className = 'token-title'; title.append(name, state);
        const info = document.createElement('div'); info.className = 'token-meta';
        info.append(meta('만든 날', fmt(token.createdAt)), meta('만료일', fmt(token.expiresAt)));
        if (token.lastUsedAt) info.append(meta('마지막 사용', fmt(token.lastUsedAt)));
        detail.append(title, info);

        const actions = document.createElement('div'); actions.className = 'token-actions';
        const revoke = document.createElement('button'); revoke.type = 'button'; revoke.className = 'text-button revoke-token'; revoke.textContent = '폐기';
        revoke.addEventListener('click', async () => {
          const ok = await confirmDialog({
            title: '토큰을 폐기할까요?',
            message: `${token.name || '이 토큰'}을(를) 폐기하면 이 토큰으로 연결한 CLI·MCP·스크립트가 바로 끊어져요.`,
            confirmLabel: '폐기', danger: true,
          });
          if (!ok || current !== epoch || !dialog.open) return;
          revoke.disabled = true;
          try {
            await api(`/api/tokens/${encodeURIComponent(token.id)}`, { method: 'DELETE' });
            if (current !== epoch) return;
            clearSecret();
            notify('토큰을 폐기했어요. 이 토큰으로 더 이상 연결할 수 없어요.');
            await load();
          } catch (error) { if (current === epoch) $('token-status').textContent = error.message; }
          finally { revoke.disabled = false; }
        });
        actions.append(revoke);
        row.append(detail, actions); list.append(row);
      }
      $('token-status').textContent = '';
    } catch (error) { if (current === epoch) $('token-status').textContent = error.message; }
  };
  const loadGrants = async () => {
    const current = epoch;
    try {
      const { grants } = await api('/api/oauth/grants');
      if (current !== epoch || !dialog.open) return;
      const list = $('grant-list');
      list.replaceChildren();
      $('grant-count').textContent = grants.length ? `${grants.length}개` : '';
      if (!grants.length) {
        const empty = document.createElement('p');
        empty.className = 'helper';
        empty.textContent = '아직 연결된 앱이 없어요. 위 주소로 연결하면 여기에 나타나요.';
        list.append(empty);
      }
      for (const grant of grants) {
        const row = document.createElement('div'); row.className = 'token-row';
        const detail = document.createElement('div'); detail.className = 'token-detail';
        const name = document.createElement('strong'); name.textContent = grant.client;
        const title = document.createElement('div'); title.className = 'token-title'; title.append(name);
        const info = document.createElement('div'); info.className = 'token-meta';
        info.append(meta('연결한 날', fmt(grant.createdAt)));
        if (grant.lastUsedAt) info.append(meta('마지막 사용', fmt(grant.lastUsedAt)));
        detail.append(title, info);
        const actions = document.createElement('div'); actions.className = 'token-actions';
        const revoke = document.createElement('button'); revoke.type = 'button'; revoke.className = 'text-button revoke-token'; revoke.textContent = '연결 끊기';
        revoke.addEventListener('click', async () => {
          const ok = await confirmDialog({
            title: `${grant.client} 연결을 끊을까요?`,
            message: '이 앱은 다시 허용하기 전까지 내 기기에 접근할 수 없어요.',
            confirmLabel: '연결 끊기', danger: true,
          });
          if (!ok || current !== epoch || !dialog.open) return;
          revoke.disabled = true;
          try {
            await api(`/api/oauth/grants/${encodeURIComponent(grant.id)}`, { method: 'DELETE' });
            if (current !== epoch) return;
            notify('연결을 끊었어요. 이 앱은 다시 허용하기 전까지 접근할 수 없어요.');
            await loadGrants();
          } catch (error) { if (current === epoch) notify(error.message); }
          finally { revoke.disabled = false; }
        });
        actions.append(revoke);
        row.append(detail, actions); list.append(row);
      }
    } catch (error) { if (current === epoch) $('grant-count').textContent = ''; }
  };
  const flash = (button, label) => {
    if (!button) return;
    const original = button.dataset.label || (button.dataset.label = button.textContent);
    button.textContent = label;
    clearTimeout(Number(button.dataset.timer));
    button.dataset.timer = String(setTimeout(() => { button.textContent = original; }, 1800));
  };
  const copy = async (text, button) => {
    try { await navigator.clipboard.writeText(text); flash(button, '복사됨'); notify('복사했어요.'); }
    catch { flash(button, '직접 복사'); notify('자동 복사를 사용할 수 없어요. 내용을 선택해 복사해 주세요.'); }
  };
  $('open-tokens').addEventListener('click', () => {
    if (dialog.open) return;
    reset(); dialog.showModal(); dialog.scrollTop = 0; void load(); void loadGrants();
  });
  $('close-tokens').addEventListener('click', () => dialog.close());
  dialog.addEventListener('close', reset);
  $('refresh-tokens').addEventListener('click', () => { void load(); void loadGrants(); });
  $('copy-token').addEventListener('click', (event) => { void copy($('token-secret').value, event.currentTarget); });
  for (const button of dialog.querySelectorAll('.copy-code')) {
    button.addEventListener('click', () => { void copy($(button.dataset.copy).textContent, button); });
  }
  $('token-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    if (busy) return;
    busy = true;
    const current = epoch;
    $('create-token').disabled = true;
    $('token-status').textContent = '토큰을 만들고 있어요.';
    try {
      const result = await api('/api/tokens', { method: 'POST', body: { name: $('token-name').value.trim(), expiresInDays: Number(days.value) } });
      if (current !== epoch || !dialog.open) return;
      secret = result.token;
      $('token-secret').value = result.token;
      $('new-token').hidden = false;
      fillSnippets();
      $('token-status').textContent = '';
      $('token-name').value = '';
      setStep(2);
      notify('토큰을 만들었어요. 아래에서 사용할 도구를 고르세요.');
      await load();
    } catch (error) { if (current === epoch) $('token-status').textContent = error.message; }
    finally { if (current === epoch) { busy = false; $('create-token').disabled = false; } }
  });
  fillSnippets();
  return { close() { if (dialog.open) dialog.close(); reset(); } };
}
