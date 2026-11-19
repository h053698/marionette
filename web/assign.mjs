const $ = (id) => document.getElementById(id);

// Member device chooser: virtual machine (any free one) or one specific physical device.
export function setupAssign({ api, notify, onChoose }) {
  const dialog = $('assign-dialog');
  let pool = null, kind = 'virtual', deviceId = '', loading = false, submitting = false, epoch = 0;

  const count = (summary) => `${summary.total}대 중 ${summary.available}대 사용 가능`;
  function option(id, title, detail, selected, disabled, onPick) {
    const button = document.createElement('button');
    button.type = 'button'; button.className = 'choice'; button.disabled = disabled || submitting;
    button.setAttribute('role', 'radio'); button.setAttribute('aria-checked', String(selected));
    const copy = document.createElement('span'); copy.className = 'choice-copy';
    const strong = document.createElement('strong'); strong.textContent = title;
    const span = document.createElement('span'); span.textContent = detail;
    copy.append(strong, span); button.append(copy);
    button.addEventListener('click', onPick);
    return button;
  }
  const model = (device) => (device.name || device.model || 'Android 기기').replaceAll('_', ' ');
  const title = (device, index) => (kind === 'virtual' ? `가상머신 ${index + 1}` : model(device));
  const detail = (device) => [kind === 'virtual' ? model(device) : '', device.androidVersion ? `Android ${device.androidVersion}` : '', device.width && device.height ? `${device.width}×${device.height}` : '', '사용 가능'].filter(Boolean).join(' · ');
  function render() {
    const virtual = pool?.virtual || { total: 0, available: 0 }, physical = pool?.physical || { total: 0, available: 0, devices: [] };
    const kinds = $('assign-kinds');
    kinds.replaceChildren(
      option('virtual', '가상머신', pool ? `가상머신 총 ${count(virtual)}` : '불러오는 중…', kind === 'virtual', !pool || virtual.available === 0, () => { kind = 'virtual'; deviceId = ''; render(); }),
      option('physical', '실물 기기', pool ? `실물 총 ${count(physical)}` : '불러오는 중…', kind === 'physical', !pool || physical.available === 0, () => { kind = 'physical'; deviceId = physical.devices?.[0]?.id || ''; render(); }),
    );
    const list = $('assign-devices');
    const choices = kind === 'virtual' ? virtual.devices || [] : physical.devices || [];
    list.hidden = !choices.length;
    list.setAttribute('aria-label', kind === 'virtual' ? '사용할 가상머신' : '사용할 실물 기기');
    const auto = kind === 'virtual' && choices.length > 1
      ? [option('', '아무거나 자동 배정', `비어 있는 ${choices.length}대 중 한 대를 골라 드려요`, deviceId === '', false, () => { deviceId = ''; render(); })]
      : [];
    list.replaceChildren(...auto, ...choices.map((device, index) => option(device.id, title(device, index), detail(device), deviceId === device.id, false, () => { deviceId = device.id; render(); })));
    $('assign-note').textContent = !pool ? '' : kind === 'virtual'
      ? virtual.available ? (choices.length > 1 ? '가상머신을 직접 고르거나 자동으로 배정받을 수 있어요.' : '비어 있는 가상머신 한 대를 배정해요.') : '지금 사용할 수 있는 가상머신이 없어요.'
      : physical.available ? '사용할 실물 기기를 선택해 주세요.' : '지금 사용할 수 있는 실물 기기가 없어요.';
    if (pool?.partial) $('assign-note').textContent += ' 일부 에이전트에 연결할 수 없어 목록이 일부일 수 있어요.';
    $('assign-confirm').disabled = submitting || !pool || (kind === 'virtual' ? virtual.available === 0 : !deviceId);
    $('assign-confirm').textContent = submitting ? '배정 중…' : '배정받기';
  }
  async function load() {
    const version = ++epoch;
    loading = true; pool = null; render();
    try {
      const result = await api('/api/pool');
      if (version !== epoch) return;
      pool = result;
      if (pool.virtual.available > 0) { kind = 'virtual'; deviceId = ''; }
      else if (pool.physical.available > 0) { kind = 'physical'; deviceId = pool.physical.devices?.[0]?.id || ''; }
    } catch (error) { if (version === epoch) { notify(error.message || '기기 현황을 불러오지 못했어요.'); } }
    finally { if (version === epoch) { loading = false; render(); } }
  }
  $('assign-confirm').addEventListener('click', async () => {
    if (submitting || !pool) return;
    submitting = true; render();
    try {
      await onChoose(kind === 'virtual' && !deviceId ? { kind: 'virtual' } : { deviceId });
      dialog.close();
    } catch (error) { if (error?.name !== 'AbortError') notify(error.message || '기기를 배정받지 못했어요.'); void load(); }
    finally { submitting = false; render(); }
  });
  $('close-assign').addEventListener('click', () => dialog.close());
  dialog.addEventListener('click', (event) => { if (event.target === dialog) dialog.close(); });
  dialog.addEventListener('close', () => { epoch++; });
  return { open() { if (dialog.open) return; kind = 'virtual'; deviceId = ''; dialog.showModal(); void load(); }, close() { if (dialog.open) dialog.close(); } };
}
