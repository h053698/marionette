export const MAX_PACKET_SIZE = 16 * 1024 * 1024;

export function parsePacket(buffer) {
  if (!(buffer instanceof ArrayBuffer) || buffer.byteLength < 5) {
    throw new Error('영상 패킷 헤더가 올바르지 않아요.');
  }
  const view = new DataView(buffer);
  const type = view.getUint8(0);
  const length = view.getUint32(1, false);
  if (length > MAX_PACKET_SIZE || buffer.byteLength !== length + 5) {
    throw new Error('영상 패킷 길이가 올바르지 않아요.');
  }
  const payload = new Uint8Array(buffer, 5);
  switch (type) {
    case 1: {
      const metadata = JSON.parse(new TextDecoder().decode(payload));
      for (const field of ['width', 'height', 'displayWidth', 'displayHeight']) {
        if (!Number.isInteger(metadata[field]) || metadata[field] < 1 || metadata[field] > 16384) {
          throw new Error('화면 크기가 올바르지 않아요.');
        }
      }
      if (!/^avc1\.[0-9a-f]{6}$/i.test(metadata.codec)) {
        throw new Error('H.264 코덱 정보가 올바르지 않아요.');
      }
      return { type, metadata };
    }
    case 2:
      return { type, data: payload };
    case 3: {
      if (length < 10) throw new Error('영상 프레임이 잘렸어요.');
      const timestamp = view.getBigUint64(5, false);
      if (timestamp > BigInt(Number.MAX_SAFE_INTEGER)) throw new Error('영상 타임스탬프 범위를 초과했어요.');
      return { type, timestamp: Number(timestamp), key: Boolean(view.getUint8(13) & 1), data: payload.subarray(9) };
    }
    case 4:
      return { type, error: JSON.parse(new TextDecoder().decode(payload)).error || '기기 영상 오류' };
    default:
      throw new Error(`알 수 없는 영상 패킷이에요: ${type}`);
  }
}

export function clientToDevice(clientX, clientY, rect, metadata) {
  if (rect.width <= 0 || rect.height <= 0) throw new Error('화면이 아직 준비되지 않았어요.');
  return {
    x: Math.max(0, Math.min(metadata.displayWidth - 1,
      Math.floor((clientX - rect.left) / rect.width * metadata.displayWidth))),
    y: Math.max(0, Math.min(metadata.displayHeight - 1,
      Math.floor((clientY - rect.top) / rect.height * metadata.displayHeight))),
  };
}

export function annexBKeyframe(config, frame) {
  if (!config?.length) return frame;
  const result = new Uint8Array(config.length + frame.length);
  result.set(config);
  result.set(frame, config.length);
  return result;
}
