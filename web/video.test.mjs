import test, { beforeEach, afterEach } from 'node:test';
import assert from 'node:assert/strict';
import { VideoPlayer } from './video.mjs';

const original = Object.fromEntries(['VideoDecoder', 'EncodedVideoChunk', 'WebSocket', 'location'].map((key) => [key, globalThis[key]]));
const metadata = { width: 360, height: 640, displayWidth: 720, displayHeight: 1280, rotation: 0, codec: 'avc1.42C029' };
let decoders, sockets;
function packet(type, payload) {
  const data = payload instanceof Uint8Array ? payload : new TextEncoder().encode(JSON.stringify(payload));
  const result = new Uint8Array(data.length + 5);
  result[0] = type;
  new DataView(result.buffer).setUint32(1, data.length);
  result.set(data, 5);
  return result.buffer;
}
function frame(key) { return packet(3, new Uint8Array([0, 0, 0, 0, 0, 0, 0, 1, key ? 1 : 0, 0, 0, 0, 1, key ? 0x65 : 0x41])); }
const tick = () => new Promise((resolve) => setImmediate(resolve));
function canvas() { return { width: 0, height: 0, getContext: () => ({ drawImage() {} }) }; }
beforeEach(() => {
  decoders = []; sockets = [];
  globalThis.location = { protocol: 'http:', host: 'localhost:8080' };
  globalThis.EncodedVideoChunk = class { constructor(options) { Object.assign(this, options); } };
  globalThis.VideoDecoder = class {
    static isConfigSupported = async () => ({ supported: true });
    constructor(callbacks) { this.callbacks = callbacks; this.state = 'unconfigured'; this.decodeQueueSize = 0; this.chunks = []; this.closes = 0; decoders.push(this); }
    configure(config) { this.config = config; this.state = 'configured'; }
    close() { this.closes++; this.state = 'closed'; }
    reset() { this.decodeQueueSize = 0; this.state = 'unconfigured'; this.chunks = []; }
    decode(chunk) { this.chunks.push(chunk); }
  };
  globalThis.WebSocket = class {
    constructor(url) { this.url = url; this.closes = 0; sockets.push(this); }
    close() { this.closes++; }
  };
});
afterEach(() => { for (const [key, value] of Object.entries(original)) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; } });

test('a keyframe arriving during asynchronous decoder setup is retained with SPS/PPS', async () => {
  let ready;
  VideoDecoder.isConfigSupported = () => new Promise((resolve) => { ready = resolve; });
  const player = new VideoPlayer(canvas());
  player.start('/preview');
  player.packet(packet(1, metadata));
  player.packet(packet(2, new Uint8Array([0, 0, 0, 1, 0x67, 0x42])));
  player.packet(frame(true)); player.packet(frame(false));
  ready({ supported: true }); await tick();
  assert.deepEqual(decoders[0].chunks.map((chunk) => chunk.type), ['key', 'delta']);
  assert.equal(decoders[0].chunks[0].data[4], 0x67);
  player.close();
});

test('a rotation supersedes an unfinished decoder setup and closes the active decoder', async () => {
  const ready = [];
  VideoDecoder.isConfigSupported = () => new Promise((resolve) => ready.push(resolve));
  const player = new VideoPlayer(canvas()); player.start('/preview');
  player.packet(packet(1, metadata));
  player.packet(packet(1, { ...metadata, width: 640, height: 360, rotation: 1 }));
  ready[1]({ supported: true }); await tick();
  ready[0]({ supported: true }); await tick();
  assert.equal(decoders.length, 1);
  assert.equal(decoders[0].config.codedWidth, 640);
  player.close();
  assert.equal(decoders[0].closes, 1);
  assert.equal(sockets[0].closes, 1);
});

test('decoder backlog drops dependent frames until the next keyframe', async () => {
  let requests = 0;
  const player = new VideoPlayer(canvas(), { requestKeyframe: () => { requests++; } }); player.start('/preview');
  player.packet(packet(1, metadata)); await tick(); player.packet(frame(true));
  decoders[0].decodeQueueSize = 5;
  player.packet(frame(false)); player.packet(frame(false));
  assert.equal(decoders[0].chunks.length, 0);
  assert.equal(requests, 2);
  player.packet(frame(true));
  assert.equal(decoders[0].chunks[0].type, 'key');
  player.close();
});

test('rendered frames are closed even when the consumer callback throws', async () => {
  const player = new VideoPlayer(canvas(), { onFrame: () => { throw new Error('consumer failure'); } }); player.start('/preview');
  player.packet(packet(1, metadata)); await tick();
  let closed = 0;
  const image = { displayWidth: 360, displayHeight: 640, close: () => { closed++; } };
  assert.throws(() => decoders[0].callbacks.output(image), /consumer failure/);
  assert.equal(closed, 1);
  player.close();
});

test('closing during setup prevents decoder creation and closes the subscription', async () => {
  let ready;
  VideoDecoder.isConfigSupported = () => new Promise((resolve) => { ready = resolve; });
  const player = new VideoPlayer(canvas()); player.start('/preview');
  player.packet(packet(1, metadata)); player.close();
  ready({ supported: true }); await tick();
  assert.equal(decoders.length, 0);
  assert.equal(sockets[0].closes, 1);
});
