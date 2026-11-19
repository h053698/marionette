import { parsePacket, annexBKeyframe } from './protocol.mjs';
import { wsUrl } from './config.mjs';

// One decoder per visible screen. Closing a player releases both the decoder
// and the server subscription; a still image can remain while it is paused.
export class VideoPlayer {
  constructor(canvas, { onState = () => {}, onFrame = () => {}, requestKeyframe = () => {}, onUnauthorized = () => {} } = {}) {
    this.canvas = canvas;
    this.context = canvas.getContext('2d', { alpha: false });
    this.onState = onState;
    this.onFrame = onFrame;
    this.requestKeyframe = requestKeyframe;
    this.onUnauthorized = onUnauthorized;
    this.metadata = null;
    this.frames = 0;
    this.generation = 0;
    this.connection = 0;
    this.stopped = true;
    this.decoder = null;
    this.ws = null;
    this.timer = null;
    this.fps = 0;
  }

  start(path) {
    this.close();
    this.stopped = false;
    this.path = path;
    this.attempts = 0;
    this.open();
  }

  open() {
    if (this.stopped) return;
    const connection = ++this.connection;
    this.onState('connecting');
    const ws = new WebSocket(wsUrl(this.path));
    this.ws = ws;
    ws.binaryType = 'arraybuffer';
    ws.onmessage = ({ data }) => {
      if (this.stopped || connection !== this.connection) return;
      try { this.packet(data); }
      catch (error) { this.fail(error.message, ws); }
    };
    ws.onerror = () => {};
    ws.onclose = (event) => {
      if (this.stopped || connection !== this.connection) return;
      this.disposeDecoder();
      this.ws = null;
      if ([1008, 4001, 4003].includes(event.code)) {
        this.stopped = true;
        this.onState('error', '접근 권한 또는 기기 점유가 종료됐어요.');
        this.onUnauthorized();
        return;
      }
      if (this.attempts >= 6) {
        this.stopped = true;
        this.onState('error', '화면에 연결할 수 없어요. 새로고침해 주세요.');
        return;
      }
      this.onState('retrying', '다시 연결 중');
      this.timer = setTimeout(() => this.open(), Math.min(15000, 1000 * 2 ** this.attempts++));
    };
  }

  fail(message, socket = this.ws) {
    this.onState('error', message);
    socket?.close();
  }

  disposeDecoder() {
    this.generation++;
    if (this.decoder && this.decoder.state !== 'closed') this.decoder.close();
    this.decoder = null;
    this.config = null;
    this.pending = [];
    this.waiting = true;
    this.configuring = false;
  }

  close() {
    this.stopped = true;
    this.connection++;
    clearTimeout(this.timer);
    if (this.ws) {
      this.ws.onclose = null;
      this.ws.close();
      this.ws = null;
    }
    this.disposeDecoder();
    this.metadata = null;
  }

  async configure(metadata) {
    this.disposeDecoder();
    const generation = this.generation;
    this.metadata = metadata;
    this.frames = 0;
    this.statsFrames = 0;
    this.statsTime = performance.now();
    this.configuring = true;
    this.onState('configuring');
    if (!('VideoDecoder' in globalThis)) throw new Error('최신 Chrome에서 HTTPS 또는 localhost로 접속해 주세요.');
    const config = { codec: metadata.codec, codedWidth: metadata.width, codedHeight: metadata.height, optimizeForLatency: true };
    const support = await VideoDecoder.isConfigSupported(config);
    if (this.stopped || generation !== this.generation) return;
    if (!support.supported) throw new Error('이 브라우저에서 기기의 H.264 영상을 재생할 수 없어요.');
    this.decoderConfig = config;
    this.decoder = new VideoDecoder({
      output: (frame) => {
        try {
          if (this.stopped || generation !== this.generation) return;
          if (this.canvas.width !== frame.displayWidth || this.canvas.height !== frame.displayHeight) {
            this.canvas.width = frame.displayWidth;
            this.canvas.height = frame.displayHeight;
          }
          this.context.drawImage(frame, 0, 0, this.canvas.width, this.canvas.height);
          this.frames++;
          this.statsFrames++;
          if (this.frames === 1) { this.attempts = 0; this.onState('live'); }
          const now = performance.now();
          if (now - this.statsTime >= 1000) {
            this.fps = Math.round(this.statsFrames * 1000 / (now - this.statsTime));
            this.statsFrames = 0;
            this.statsTime = now;
          }
          this.onFrame({ metadata, frames: this.frames, fps: this.fps });
        } finally { frame.close(); }
      },
      error: (error) => { if (generation === this.generation) this.fail(error.message); },
    });
    this.decoder.configure(config);
    this.configuring = false;
    const pending = this.pending;
    this.pending = [];
    for (const packet of pending) this.decode(packet);
    if (this.waiting) this.requestKeyframe();
  }

  packet(buffer) {
    const packet = parsePacket(buffer);
    if (packet.type === 1) {
      const configuring = this.configure(packet.metadata);
      const generation = this.generation;
      configuring.catch((error) => {
        if (this.stopped || generation !== this.generation) return;
        this.onState('error', error.message);
        // Unsupported codecs should not create a reconnect loop.
        this.close();
      });
    } else if (packet.type === 2) {
      this.config = packet.data;
    } else if (packet.type === 4) {
      throw new Error(packet.error);
    } else if (packet.type === 3) {
      if (this.configuring) {
        if (packet.key) this.pending = [packet];
        else if (this.pending.length && this.pending.length < 24) this.pending.push(packet);
        else this.pending = [];
      } else this.decode(packet);
    }
  }

  decode(packet) {
    if (this.decoder?.state !== 'configured') return;
    if (this.decoder.decodeQueueSize > 4) {
      this.decoder.reset();
      this.decoder.configure(this.decoderConfig);
      this.waiting = true;
      this.requestKeyframe();
    }
    if (this.waiting && !packet.key) return;
    const data = packet.key ? annexBKeyframe(this.config, packet.data) : packet.data;
    this.decoder.decode(new EncodedVideoChunk({ type: packet.key ? 'key' : 'delta', timestamp: packet.timestamp, data }));
    this.waiting = false;
  }
}
