// 확인·입력을 인라인 버튼 대신 팝업으로 받는 공용 도우미예요.
// confirmDialog → true/false, promptDialog → 문자열 또는 null, choiceDialog → 선택값 또는 null.
const $ = (id) => document.getElementById(id);
let queue = Promise.resolve();

function ask(options) {
  const run = () => new Promise((resolve) => {
    const dialog = $('prompt-dialog'), form = $('prompt-form'), input = $('prompt-input');
    const submit = $('prompt-submit'), choiceActions = $('prompt-choices');
    const { title, message = '', confirmLabel = '확인', cancelLabel = '취소', danger = false, label = '', placeholder = '', value = '', maxLength = 100, validate, choices } = options;
    const wantsInput = Boolean(options.input);
    let settled = false;
    const finish = (result) => {
      if (settled) return;
      settled = true;
      cleanup();
      if (dialog.open) dialog.close();
      resolve(result);
    };
    const onSubmit = (event) => {
      event.preventDefault();
      if (choices) return;
      if (!wantsInput) { finish(true); return; }
      const text = input.value.trim();
      const problem = validate ? validate(text) : (text ? '' : '값을 입력해 주세요.');
      if (problem) { $('prompt-error').textContent = problem; $('prompt-error').hidden = false; input.focus(); return; }
      finish(text);
    };
    const onCancel = () => finish(wantsInput || choices ? null : false);
    const onBackdrop = (event) => { if (event.target === dialog) onCancel(); };
    const onInput = () => { $('prompt-error').hidden = true; };
    function cleanup() {
      form.removeEventListener('submit', onSubmit);
      $('prompt-cancel').removeEventListener('click', onCancel);
      $('prompt-close').removeEventListener('click', onCancel);
      dialog.removeEventListener('close', onCancel);
      dialog.removeEventListener('cancel', onCancel);
      dialog.removeEventListener('click', onBackdrop);
      input.removeEventListener('input', onInput);
      choiceActions.replaceChildren();
    }

    $('prompt-title').textContent = title;
    $('prompt-message').textContent = message;
    $('prompt-message').hidden = !message;
    $('prompt-field').hidden = !wantsInput;
    $('prompt-label').textContent = label;
    input.value = wantsInput ? value : ''; input.placeholder = placeholder; input.maxLength = maxLength;
    $('prompt-error').hidden = true; $('prompt-error').textContent = '';
    $('prompt-cancel').textContent = cancelLabel;
    submit.textContent = confirmLabel;
    submit.className = `button ${danger ? 'danger' : 'primary'}`;
    submit.hidden = Boolean(choices);
    if (choices) {
      for (const choice of choices) {
        const button = document.createElement('button');
        button.type = 'button';
        button.className = `button${choice.danger ? ' danger' : ''}`;
        button.textContent = choice.label;
        button.addEventListener('click', () => finish(choice.value));
        choiceActions.append(button);
      }
    }

    form.addEventListener('submit', onSubmit);
    $('prompt-cancel').addEventListener('click', onCancel);
    $('prompt-close').addEventListener('click', onCancel);
    dialog.addEventListener('close', onCancel);
    dialog.addEventListener('cancel', onCancel);
    dialog.addEventListener('click', onBackdrop);
    input.addEventListener('input', onInput);
    dialog.showModal();
    if (wantsInput) { input.focus(); input.select(); } else if (danger || choices) $('prompt-cancel').focus(); else submit.focus();
  });
  const result = queue.then(run);
  queue = result.catch(() => {});
  return result;
}

export function confirmDialog(options) { return ask({ ...options, input: false }); }
export function promptDialog(options) { return ask({ ...options, input: true }); }
export function choiceDialog(options) { return ask({ ...options, input: false }); }
