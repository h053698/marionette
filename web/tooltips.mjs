// Render in the top layer so tooltips remain visible above modal device dialogs.
export function setupTooltips() {
  const generated = new WeakMap();
  const tooltip = document.createElement('div');
  tooltip.id = 'control-tooltip'; tooltip.className = 'control-tooltip'; tooltip.setAttribute('role', 'tooltip');
  tooltip.hidden = true;
  const popover = typeof tooltip.showPopover === 'function';
  if (popover) tooltip.setAttribute('popover', 'manual');
  document.body.append(tooltip);
  let scheduled = false, active, candidate, timer, savedTitle;
  const selector = 'button, a[href], input[type="range"], [data-tooltip], [title]';
  function label(element) {
    return element.dataset.tooltip || element.title || element.getAttribute('aria-label')
      || element.labels?.[0]?.textContent?.trim() || element.textContent.replace(/\s+/g, ' ').trim();
  }
  function hide() {
    clearTimeout(timer); candidate = null;
    if (popover && tooltip.matches(':popover-open')) tooltip.hidePopover();
    tooltip.hidden = true;
    if (active) {
      const ids = (active.getAttribute('aria-describedby') || '').split(/\s+/).filter((id) => id && id !== tooltip.id);
      if (ids.length) active.setAttribute('aria-describedby', ids.join(' ')); else active.removeAttribute('aria-describedby');
      if (savedTitle) active.title = savedTitle;
    }
    active = null; savedTitle = '';
  }
  function show(element) {
    if (!element.isConnected || !label(element)) return;
    const text = label(element).slice(0, 400);
    hide(); active = candidate = element; savedTitle = element.title;
    element.removeAttribute('title');
    tooltip.textContent = text;
    (popover ? document.body : element.closest('dialog[open]') || document.body).append(tooltip);
    const ids = new Set((element.getAttribute('aria-describedby') || '').split(/\s+/).filter(Boolean));
    ids.add(tooltip.id); element.setAttribute('aria-describedby', [...ids].join(' '));
    tooltip.hidden = false;
    if (popover) tooltip.showPopover();
    tooltip.style.left = '0px'; tooltip.style.top = '0px';
    const origin = tooltip.getBoundingClientRect(), target = element.getBoundingClientRect();
    const width = origin.width, height = origin.height;
    const left = Math.max(8, Math.min(innerWidth - width - 8, target.left + (target.width - width) / 2));
    const below = target.bottom + 7;
    const top = Math.max(8, below + height <= innerHeight - 8 ? below : target.top - height - 7);
    tooltip.style.left = `${left - origin.left}px`; tooltip.style.top = `${top - origin.top}px`;
  }
  function schedule(element, immediate = false) {
    if (!element || element === candidate) return;
    hide(); candidate = element;
    if (immediate) show(element); else timer = setTimeout(() => show(element), 220);
  }
  function update() {
    scheduled = false;
    if (active && !active.isConnected) hide();
    for (const element of document.querySelectorAll('button, a[href], input[type="range"]')) {
      if (element === active) continue;
      if (element.title && generated.get(element) !== element.title) continue;
      const text = element.dataset.tooltip || element.getAttribute('aria-label') || element.labels?.[0]?.textContent?.trim()
        || element.textContent.replace(/\s+/g, ' ').trim();
      if (text) { element.title = text.slice(0, 400); generated.set(element, element.title); }
    }
  }
  document.addEventListener('pointerover', (event) => {
    if (event.pointerType !== 'mouse' && event.pointerType !== 'pen') return;
    schedule(event.target.closest(selector));
  }, true);
  document.addEventListener('pointerout', (event) => {
    if (candidate && !candidate.contains(event.relatedTarget)) hide();
  }, true);
  document.addEventListener('focusin', (event) => schedule(event.target.closest(selector), true));
  document.addEventListener('focusout', hide);
  document.addEventListener('pointerdown', hide, true);
  document.addEventListener('click', hide, true);
  document.addEventListener('scroll', hide, true);
  document.addEventListener('close', hide, true);
  document.addEventListener('keydown', (event) => { if (event.key === 'Escape') hide(); });
  window.addEventListener('resize', hide);
  const observer = new MutationObserver(() => { if (!scheduled) { scheduled = true; requestAnimationFrame(update); } });
  observer.observe(document.body, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ['aria-label'] });
  update();
}
