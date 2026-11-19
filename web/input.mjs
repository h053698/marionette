import { wsUrl } from './config.mjs';

// Inputs are never replayed after reconnect. Movement samples do not wait for
// acknowledgements; both ends keep just the latest adjacent sample.
export class RealtimeInput {
  constructor({ onState = () => {}, onUnauthorized = () => {}, onError = () => {} } = {}) {
    Object.assign(this, { onState, onUnauthorized, onError });
    this.pending = new Map(); this.sequence = 0; this.stopped = true;
  }
  start(path, token) {
    this.close(); this.path = path; this.token = token; this.stopped = false; this.attempt = 0; this.connect();
  }
  connect() {
    if (this.stopped) return;
    const socket = this.socket = new WebSocket(wsUrl(this.path));
    this.ready = false;
    this.handshake = setTimeout(() => socket.close(), 5000);
    socket.onopen = () => socket.send(JSON.stringify({ type: 'auth', token: this.token }));
    socket.onmessage = ({ data }) => {
      if (socket !== this.socket || this.stopped) return;
      let result; try { result = JSON.parse(data); } catch { socket.close(); return; }
      if (result.type === 'ready') {
        clearTimeout(this.handshake); this.ready = true; this.attempt = 0; this.onState(true); return;
      }
      if (result.type === 'error') {
        if ([401, 403, 410].includes(result.status)) { this.close(); this.onUnauthorized(); }
        else this.onError(new Error(result.error || '입력 연결을 준비할 수 없어요.'));
        return;
      }
      const pending = this.pending.get(result.id);
      if (!pending) return;
      this.pending.delete(result.id); clearTimeout(pending.timer);
      if (result.ok === false) pending.reject(new Error(result.error || '기기 입력에 실패했어요.'));
      else pending.resolve(result);
    };
    socket.onclose = ({ code }) => {
      if (socket !== this.socket || this.stopped) return;
      clearTimeout(this.handshake); this.ready = false; this.latest = null;
      this.rejectPending(new Error('입력 연결이 끊겼어요. 다시 연결하고 있어요.'));
      this.onState(false);
      if ([1008, 4001, 4003].includes(code)) { this.close(); this.onUnauthorized(); return; }
      this.retry = setTimeout(() => this.connect(), Math.min(5000, 300 * 2 ** Math.min(this.attempt++, 5)));
    };
  }
  write(body) {
    if (!this.ready || this.socket?.readyState !== WebSocket.OPEN) throw new Error('입력 연결을 준비 중이에요. 잠시 후 다시 시도해 주세요.');
    if (this.socket.bufferedAmount > 64 * 1024) throw new Error('입력 연결이 혼잡해요. 잠시 후 다시 시도해 주세요.');
    const id = `i${++this.sequence}`;
    this.socket.send(JSON.stringify({ ...body, id }));
    return id;
  }
  request(body) {
    return new Promise((resolve, reject) => {
      try {
        this.flush();
        const id = this.write(body);
        const timer = setTimeout(() => {
          this.pending.delete(id); reject(new Error('기기 입력 응답이 지연되어 연결을 다시 준비합니다.'));
          this.socket?.close();
        }, 10000);
        this.pending.set(id, { resolve, reject, timer });
      } catch (error) { reject(error); }
    });
  }
  move(body) {
    if (!this.ready) return;
    this.latest = body;
    if (!this.frame) this.frame = requestAnimationFrame(() => { this.frame = null; this.flush(); });
  }
  flush() {
    if (!this.latest) return;
    const body = this.latest; this.latest = null;
    try { this.write(body); } catch { /* Stale movement is discarded under pressure. */ }
  }
  rejectPending(error) {
    for (const pending of this.pending.values()) { clearTimeout(pending.timer); pending.reject(error); }
    this.pending.clear();
  }
  close() {
    this.stopped = true; this.ready = false; this.latest = null;
    clearTimeout(this.retry); clearTimeout(this.handshake); cancelAnimationFrame(this.frame); this.frame = null;
    const socket = this.socket; this.socket = null; socket?.close();
    this.rejectPending(new DOMException('Input connection closed', 'AbortError'));
  }
}
