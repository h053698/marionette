package dev.marionette;

import android.media.MediaCodec;
import android.media.MediaCodecInfo;
import android.media.MediaFormat;
import android.os.Bundle;
import android.view.Surface;
import java.io.ByteArrayOutputStream;
import java.nio.ByteBuffer;
import java.util.Locale;
import java.util.concurrent.atomic.AtomicBoolean;
import org.json.JSONObject;

final class Encoder implements Runnable {
    final Platform platform;
    private static final class Profile {
        final int maxSize, bitrate, fps;
        Profile(int maxSize, int bitrate, int fps) {
            this.maxSize = maxSize; this.bitrate = bitrate; this.fps = fps;
        }
    }
    private volatile Profile profile;
    final AtomicBoolean idr = new AtomicBoolean(true);
    volatile boolean running = true;
    volatile JSONObject metadata;
    volatile String failure;
    private int syncRecoveryAttempts;
    VideoHub hub;
    Encoder(Platform platform, int maxSize, int bitrate, int fps) {
        this.platform = platform; this.profile = new Profile(maxSize, bitrate, fps);
    }
    synchronized void videoProfile(String name) {
        if (failure != null || !running) throw new IllegalStateException("Encoder unavailable: " + failure);
        Profile next;
        if ("preview".equals(name)) next = new Profile(480, 600000, 8);
        else if ("interactive".equals(name)) next = new Profile(1280, 4000000, 30);
        else throw new IllegalArgumentException("profile must be preview or interactive");
        Profile current = profile;
        if (current.maxSize != next.maxSize || current.bitrate != next.bitrate || current.fps != next.fps)
            profile = next;
    }
    void requestKeyframe() { idr.set(true); }
    void stop() { running = false; }

    public void run() {
        try {
            while (running) capture(platform.geometry());
        } catch (Exception e) {
            failure = "Screen capture failed: " + Main.message(e);
            metadata = null;
            System.err.println(failure);
            hub.error(failure);
        }
    }

    private void capture(Platform.Geometry geometry) throws Exception {
        Profile active = profile;
        int maxSize = active.maxSize, bitrate = active.bitrate, fps = active.fps;
        double scale = Math.min(1.0, (double) maxSize / Math.max(geometry.width, geometry.height));
        MediaCodec codec = MediaCodec.createEncoderByType("video/avc");
        Surface surface = null;
        Platform.Mirror mirror = null;
        boolean started = false;
        try {
            MediaCodecInfo.VideoCapabilities caps = codec.getCodecInfo().getCapabilitiesForType("video/avc").getVideoCapabilities();
            int wa = Math.max(2, caps.getWidthAlignment()), ha = Math.max(2, caps.getHeightAlignment());
            int width = Math.max(wa, ((int) (geometry.width * scale) / wa) * wa);
            int height = Math.max(ha, ((int) (geometry.height * scale) / ha) * ha);
            if (!caps.areSizeAndRateSupported(width, height, fps))
                throw new IllegalStateException("Encoder does not support " + width + "x" + height + " at " + fps + " fps");
            MediaFormat format = MediaFormat.createVideoFormat("video/avc", width, height);
            format.setInteger(MediaFormat.KEY_COLOR_FORMAT, MediaCodecInfo.CodecCapabilities.COLOR_FormatSurface);
            format.setInteger(MediaFormat.KEY_BIT_RATE, bitrate);
            format.setInteger(MediaFormat.KEY_FRAME_RATE, fps);
            format.setInteger(MediaFormat.KEY_I_FRAME_INTERVAL, 1);
            format.setInteger(MediaFormat.KEY_MAX_B_FRAMES, 0);
            format.setLong("repeat-previous-frame-after", 1000000L / fps);
            format.setFloat("max-fps-to-encoder", fps);
            codec.configure(format, null, null, MediaCodec.CONFIGURE_FLAG_ENCODE);
            surface = codec.createInputSurface();
            codec.start(); started = true;
            hub.reset(); metadata = null;
            mirror = platform.mirror(surface, geometry, width, height);
            MediaCodec.BufferInfo info = new MediaCodec.BufferInfo();
            long nextGeometry = 0;
            byte[] config = null;
            ByteArrayOutputStream partial = new ByteArrayOutputStream();
            long partialPts = 0;
            boolean partialKey = false;
            long syncDeadline = 0;
            while (running) {
                if (profile != active) return;
                long now = android.os.SystemClock.uptimeMillis();
                if (now >= nextGeometry) {
                    if (!geometry.same(platform.geometry())) return;
                    nextGeometry = now + 250;
                }
                if (idr.getAndSet(false)) {
                    Bundle params = new Bundle();
                    params.putInt(MediaCodec.PARAMETER_KEY_REQUEST_SYNC_FRAME, 0);
                    codec.setParameters(params);
                    if (syncDeadline == 0) syncDeadline = now + 2000;
                }
                if (syncDeadline != 0 && now >= syncDeadline) {
                    // Some surface encoders stop repeating an unchanged image. A sync
                    // request then waits forever for fresh input. Reattach the actual
                    // display to obtain a new real frame, without synthesizing pixels.
                    if (++syncRecoveryAttempts > 3)
                        throw new IllegalStateException("Encoder did not produce a requested keyframe after display recreation");
                    System.err.println("Recreating idle capture to satisfy keyframe request");
                    return;
                }
                int index = codec.dequeueOutputBuffer(info, 100000);
                if (index == MediaCodec.INFO_OUTPUT_FORMAT_CHANGED) {
                    MediaFormat output = codec.getOutputFormat();
                    ByteArrayOutputStream csd = new ByteArrayOutputStream();
                    for (int i = 0; i < 3; i++) {
                        ByteBuffer data = output.getByteBuffer("csd-" + i);
                        if (data != null) csd.write(annexB(copy(data)));
                    }
                    if (csd.size() > 0) { config = csd.toByteArray(); publish(geometry, width, height, config); }
                } else if (index >= 0) {
                    try {
                        ByteBuffer buffer = codec.getOutputBuffer(index);
                        if (buffer == null) throw new IllegalStateException("Encoder output buffer is null");
                        buffer.position(info.offset); buffer.limit(info.offset + info.size);
                        byte[] data = copy(buffer);
                        if ((info.flags & MediaCodec.BUFFER_FLAG_CODEC_CONFIG) != 0) {
                            config = annexB(data); publish(geometry, width, height, config);
                        } else if (data.length != 0) {
                            if (partial.size() == 0) partialPts = info.presentationTimeUs;
                            partialKey |= (info.flags & MediaCodec.BUFFER_FLAG_KEY_FRAME) != 0;
                            if (partial.size() + data.length > VideoHub.MAX_PAYLOAD - 9)
                                throw new IllegalStateException("Encoder access unit exceeds protocol limit");
                            partial.write(data);
                            if ((info.flags & MediaCodec.BUFFER_FLAG_PARTIAL_FRAME) == 0) {
                                if (config == null) throw new IllegalStateException("Encoder emitted frame without SPS/PPS");
                                if (partialKey) { syncDeadline = 0; syncRecoveryAttempts = 0; }
                                hub.frame(annexB(partial.toByteArray()), partialPts, partialKey);
                                partial.reset(); partialKey = false;
                            }
                        }
                        if ((info.flags & MediaCodec.BUFFER_FLAG_END_OF_STREAM) != 0)
                            throw new IllegalStateException("Encoder ended unexpectedly");
                    } finally { codec.releaseOutputBuffer(index, false); }
                }
            }
        } finally {
            if (mirror != null) { try { mirror.close(); } catch (Exception e) { System.err.println("Mirror cleanup: " + Main.message(e)); } }
            if (started) { try { codec.stop(); } catch (Exception ignored) { } }
            try { codec.release(); } finally { if (surface != null) surface.release(); }
        }
    }

    private void publish(Platform.Geometry g, int width, int height, byte[] config) throws Exception {
        String codecName = codecString(config);
        JSONObject value = new JSONObject().put("width", width).put("height", height)
                .put("displayWidth", g.width).put("displayHeight", g.height).put("rotation", g.rotation)
                .put("codec", codecName).put("capture", platform.captureMethod)
                .put("encoder", "MediaCodec").put("mime", "video/avc");
        hub.configure(value, config);
        metadata = value;
    }

    static byte[] copy(ByteBuffer buffer) {
        ByteBuffer view = buffer.duplicate(); byte[] bytes = new byte[view.remaining()]; view.get(bytes); return bytes;
    }

    static byte[] annexB(byte[] bytes) throws Exception {
        if (bytes.length == 0) return bytes;
        if (startCode(bytes, 0) != 0) return bytes;
        // Android AVC encoders normally produce Annex B; also accept four-byte NAL lengths.
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        int offset = 0;
        while (offset + 4 <= bytes.length) {
            int size = ((bytes[offset] & 255) << 24) | ((bytes[offset + 1] & 255) << 16)
                    | ((bytes[offset + 2] & 255) << 8) | (bytes[offset + 3] & 255);
            offset += 4;
            if (size <= 0 || size > bytes.length - offset) throw new IllegalStateException("Unsupported AVC NAL framing");
            out.write(new byte[] {0, 0, 0, 1}); out.write(bytes, offset, size); offset += size;
        }
        if (offset != bytes.length) throw new IllegalStateException("Truncated AVC NAL length");
        return out.toByteArray();
    }

    static int startCode(byte[] bytes, int i) {
        if (i + 2 < bytes.length && bytes[i] == 0 && bytes[i + 1] == 0) {
            if (bytes[i + 2] == 1) return 3;
            if (i + 3 < bytes.length && bytes[i + 2] == 0 && bytes[i + 3] == 1) return 4;
        }
        return 0;
    }

    static String codecString(byte[] config) {
        String result = null;
        boolean pps = false;
        for (int i = 0; i < config.length; i++) {
            int prefix = startCode(config, i);
            if (prefix == 0 || i + prefix >= config.length) continue;
            int nal = i + prefix;
            if ((config[nal] & 31) == 7 && nal + 3 < config.length)
                result = String.format(Locale.US, "avc1.%02X%02X%02X", config[nal + 1] & 255,
                        config[nal + 2] & 255, config[nal + 3] & 255);
            if ((config[nal] & 31) == 8) pps = true;
            i += prefix;
        }
        if (result == null || !pps) throw new IllegalStateException("Encoder configuration lacks valid SPS/PPS");
        return result;
    }
}
