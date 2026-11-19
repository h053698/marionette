package dev.marionette;

import android.net.LocalSocket;
import java.io.ByteArrayOutputStream;
import java.io.DataOutputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ArrayBlockingQueue;
import org.json.JSONObject;

/** Each writer owns a bounded queue. Overflow discards dependencies until the next IDR. */
final class VideoHub {
    static final int MAX_PAYLOAD = 16 * 1024 * 1024;
    private final List<Client> clients = new ArrayList<Client>();
    private byte[] metadata, config, error;
    private boolean closed;
    private final Runnable requestKeyframe;
    VideoHub(Runnable request) { requestKeyframe = request; }

    static byte[] packet(int type, byte[] payload) throws IOException {
        if (payload.length > MAX_PAYLOAD) throw new IOException("Video payload exceeds 16 MiB");
        ByteArrayOutputStream bytes = new ByteArrayOutputStream(payload.length + 5);
        DataOutputStream out = new DataOutputStream(bytes);
        out.writeByte(type); out.writeInt(payload.length); out.write(payload);
        return bytes.toByteArray();
    }

    synchronized void add(LocalSocket socket) throws IOException {
        if (closed || clients.size() >= 8) { socket.close(); return; }
        Client client = new Client(socket);
        clients.add(client);
        client.headers();
        if (error != null) client.queue.offer(error);
        new Thread(client, "marionette-video-writer").start();
        requestKeyframe.run();
    }

    synchronized void reset() {
        metadata = null; config = null; error = null;
        for (Client c : clients) { c.queue.clear(); c.waiting = true; }
    }

    synchronized void configure(JSONObject info, byte[] annexB) throws IOException {
        metadata = packet(1, info.toString().getBytes(StandardCharsets.UTF_8));
        config = packet(2, annexB);
        error = null;
        for (Client c : clients) { c.queue.clear(); c.waiting = true; c.headers(); }
        requestKeyframe.run();
    }

    synchronized void frame(byte[] accessUnit, long pts, boolean key) throws IOException {
        if (metadata == null || config == null) return;
        ByteArrayOutputStream bytes = new ByteArrayOutputStream(accessUnit.length + 9);
        DataOutputStream out = new DataOutputStream(bytes);
        out.writeLong(pts); out.writeByte(key ? 1 : 0); out.write(accessUnit);
        byte[] packet = packet(3, bytes.toByteArray());
        boolean dropped = false;
        for (Client c : clients) {
            if (c.waiting && !key) continue;
            if (!c.queue.offer(packet)) {
                c.queue.clear(); c.waiting = true; c.headers(); dropped = true;
                if (key) { c.queue.offer(packet); c.waiting = false; }
            } else if (key) c.waiting = false;
        }
        if (dropped) requestKeyframe.run();
    }

    synchronized void error(String message) {
        try {
            error = packet(4, new JSONObject().put("error", message).toString().getBytes(StandardCharsets.UTF_8));
            for (Client c : clients) { c.queue.clear(); c.waiting = true; c.queue.offer(error); }
        } catch (Exception e) { System.err.println(Main.message(e)); }
    }

    synchronized void close() {
        closed = true;
        for (Client c : new ArrayList<Client>(clients)) c.close();
        clients.clear();
    }

    private final class Client implements Runnable {
        final LocalSocket socket;
        // At most eight encoded packets per client; never block the encoder on socket I/O.
        final ArrayBlockingQueue<byte[]> queue = new ArrayBlockingQueue<byte[]>(8);
        boolean waiting = true;
        volatile boolean stopped;
        volatile Thread writer;
        Client(LocalSocket socket) { this.socket = socket; }
        void headers() { if (metadata != null) queue.offer(metadata); if (config != null) queue.offer(config); }
        public void run() {
            writer = Thread.currentThread();
            try {
                while (!stopped && !Thread.currentThread().isInterrupted()) {
                    byte[] bytes = queue.take();
                    socket.getOutputStream().write(bytes);
                }
            } catch (Exception ignored) {
            } finally { synchronized (VideoHub.this) { clients.remove(this); } close(); }
        }
        void close() {
            stopped = true;
            try { socket.close(); } catch (IOException ignored) { }
            if (writer != null) writer.interrupt();
        }
    }
}
