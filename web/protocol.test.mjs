import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parsePacket, clientToDevice, annexBKeyframe, MAX_PACKET_SIZE } from './protocol.mjs';

function packet(type, payload) {
  const buffer = new ArrayBuffer(5 + payload.byteLength);
  const view = new DataView(buffer);
  view.setUint8(0, type);
  view.setUint32(1, payload.byteLength, false);
  new Uint8Array(buffer, 5).set(payload);
  return buffer;
}

test('decode an access unit with a full 64-bit microsecond timestamp', () => {
  const payload = new Uint8Array(14);
  new DataView(payload.buffer).setBigUint64(0, 5_000_000_123n, false);
  payload[8] = 1;
  payload.set([0, 0, 0, 1, 0x65], 9);
  const result = parsePacket(packet(3, payload));
  assert.equal(result.timestamp, 5_000_000_123);
  assert.equal(result.key, true);
  assert.deepEqual([...result.data], [0, 0, 0, 1, 0x65]);
});

test('reject truncated frames, mismatched lengths and oversized packets', () => {
  assert.throws(() => parsePacket(new ArrayBuffer(4)));
  assert.throws(() => parsePacket(packet(3, new Uint8Array(9))));
  const truncated = packet(2, new Uint8Array(1));
  new DataView(truncated).setUint32(1, 2, false);
  assert.throws(() => parsePacket(truncated));
  new DataView(truncated).setUint32(1, MAX_PACKET_SIZE + 1, false);
  assert.throws(() => parsePacket(truncated));
});

test('reject invalid dimensions and codec metadata before configuring a decoder', () => {
  const encode = (obj) => packet(1, new TextEncoder().encode(JSON.stringify(obj)));
  const metadata = { width: 720, height: 1280, displayWidth: 1080, displayHeight: 1920, codec: 'avc1.42E01F' };
  assert.deepEqual(parsePacket(encode(metadata)).metadata, metadata);
  assert.throws(() => parsePacket(encode({ ...metadata, width: -1 })));
  assert.throws(() => parsePacket(encode({ ...metadata, codec: 'h264' })));
});

test('map a scaled screen to the physical display and clamp captured pointer releases', () => {
  const rect = { left: 100, top: 30, width: 360, height: 640 };
  const metadata = { displayWidth: 1080, displayHeight: 1920 };
  assert.deepEqual(clientToDevice(280, 350, rect, metadata), { x: 540, y: 960 });
  assert.deepEqual(clientToDevice(999, -10, rect, metadata), { x: 1079, y: 0 });
  assert.throws(() => clientToDevice(0, 0, { ...rect, width: 0 }, metadata));
});

test('prepend parameter sets to late-join keyframes without modifying their buffers', () => {
  const config = new Uint8Array([0, 0, 1, 0x67]);
  const frame = new Uint8Array([0, 0, 1, 0x65]);
  assert.deepEqual([...annexBKeyframe(config, frame)], [0, 0, 1, 0x67, 0, 0, 1, 0x65]);
  assert.equal(config.length, 4);
  assert.equal(annexBKeyframe(null, frame), frame);
});
