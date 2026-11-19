// 네이티브 <select> 대신 쓰는 선택 팝업. 버튼을 누르면 목록이 떠오르고, 키보드로도 고를 수 있어요.
// createPicker({ label, options: [{ value, label, description? }], value, onChange, placeholder })
//   -> { element, value, setValue(v), setOptions(options), setDisabled(bool) }
let counter = 0;

export function createPicker({ label, options = [], value = '', onChange = () => {}, placeholder = '선택' }) {
  const id = `picker-${++counter}`;
  let current = value, items = options, disabled = false, index = -1;
  const root = document.createElement('div');
  root.className = 'picker';
  const button = document.createElement('button');
  button.type = 'button'; button.className = 'picker-button';
  button.setAttribute('aria-haspopup', 'listbox'); button.setAttribute('aria-expanded', 'false'); button.setAttribute('aria-label', label);
  const text = document.createElement('span'); text.className = 'picker-value';
  const caret = document.createElement('span'); caret.className = 'picker-caret'; caret.setAttribute('aria-hidden', 'true'); caret.textContent = '⌄';
  button.append(text, caret);
  const list = document.createElement('ul');
  list.className = 'picker-list'; list.id = id; list.setAttribute('role', 'listbox'); list.tabIndex = -1; list.hidden = true;
  button.setAttribute('aria-controls', id);
  root.append(button, list);

  function render() {
    const selected = items.find((item) => String(item.value) === String(current));
    text.textContent = selected ? selected.label : placeholder;
    text.classList.toggle('placeholder', !selected);
    list.replaceChildren(...items.map((item, i) => {
      const li = document.createElement('li');
      li.setAttribute('role', 'option'); li.dataset.value = item.value; li.id = `${id}-${i}`;
      li.setAttribute('aria-selected', String(String(item.value) === String(current)));
      const title = document.createElement('span'); title.className = 'picker-option-label'; title.textContent = item.label; li.append(title);
      if (item.description) { const d = document.createElement('span'); d.className = 'picker-option-description'; d.textContent = item.description; li.append(d); }
      li.addEventListener('mousedown', (event) => event.preventDefault());
      li.addEventListener('click', () => choose(i));
      return li;
    }));
    button.disabled = disabled;
  }
  function highlight(next) {
    index = Math.max(0, Math.min(items.length - 1, next));
    for (const li of list.children) li.classList.remove('active');
    const li = list.children[index];
    if (li) { li.classList.add('active'); list.setAttribute('aria-activedescendant', li.id); li.scrollIntoView({ block: 'nearest' }); }
  }
  function open() {
    if (disabled || !items.length || !list.hidden) return;
    list.hidden = false; button.setAttribute('aria-expanded', 'true');
    highlight(Math.max(0, items.findIndex((item) => String(item.value) === String(current))));
    list.focus({ preventScroll: true });
    document.addEventListener('pointerdown', outside, true);
  }
  function close(refocus = false) {
    if (list.hidden) return;
    list.hidden = true; button.setAttribute('aria-expanded', 'false');
    document.removeEventListener('pointerdown', outside, true);
    if (refocus) button.focus({ preventScroll: true });
  }
  function outside(event) { if (!root.contains(event.target)) close(); }
  function choose(i) {
    const item = items[i];
    if (!item) return;
    const changed = String(item.value) !== String(current);
    current = item.value; render(); close(true);
    if (changed) onChange(current);
  }
  button.addEventListener('click', () => (list.hidden ? open() : close(true)));
  button.addEventListener('keydown', (event) => {
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') { event.preventDefault(); open(); }
  });
  list.addEventListener('keydown', (event) => {
    if (event.key === 'ArrowDown') { event.preventDefault(); highlight(index + 1); }
    else if (event.key === 'ArrowUp') { event.preventDefault(); highlight(index - 1); }
    else if (event.key === 'Home') { event.preventDefault(); highlight(0); }
    else if (event.key === 'End') { event.preventDefault(); highlight(items.length - 1); }
    else if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); choose(index); }
    else if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); close(true); }
    else if (event.key === 'Tab') close();
  });
  render();
  return {
    element: root,
    get value() { return current; },
    setValue(next) { current = next; render(); },
    setOptions(next) { items = next; render(); },
    setDisabled(next) { disabled = Boolean(next); if (disabled) close(); render(); },
  };
}
