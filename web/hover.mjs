import { clientToDevice } from './protocol.mjs';

export function setupDeviceHover(canvas, { current, send }) {
  let last;
  canvas.addEventListener('pointermove', (event) => {
    const device = current();
    if (event.pointerType !== 'mouse' || event.buttons || device?.touching || !device?.metadata) return;
    const point = clientToDevice(event.clientX, event.clientY, canvas.getBoundingClientRect(), device.metadata);
    last = { ...point, key: device.key };
    send({ action: 'hover', ...point, phase: 'move' });
  });
  const exit = () => {
    const point = last; last = null;
    if (point && current()?.key === point.key) send({ action: 'hover', x: point.x, y: point.y, phase: 'exit' });
  };
  canvas.addEventListener('pointerleave', exit);
  canvas.addEventListener('pointerdown', exit);
}
