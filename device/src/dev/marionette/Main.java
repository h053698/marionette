package dev.marionette;

import android.net.LocalServerSocket;
import android.net.LocalSocket;
import android.os.Looper;
import android.os.SystemClock;
import java.io.ByteArrayOutputStream;
import java.io.BufferedInputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.ByteBuffer;
import java.nio.charset.CodingErrorAction;
import java.nio.charset.StandardCharsets;
import java.util.Collections;
import java.util.HashSet;
import java.util.Set;
import java.util.concurrent.Semaphore;
import org.json.JSONObject;

public final class Main {
    private static final int MAX_LINE = 1024 * 1024;
    private final Set<LocalSocket> sockets = Collections.synchronizedSet(new HashSet<LocalSocket>());
    private final Semaphore connections = new Semaphore(16);
    private volatile boolean running = true;
    private LocalServerSocket server;
    private Platform platform;
    private Encoder encoder;
    private VideoHub hub;
    private Controls controls;
    private Thread encoderThread;
    private String startupFailure;

    public static void main(String[] args) {
        try { new Main().run(args); }
        catch (Exception e) { System.err.println("Marionette fatal: " + message(e)); System.exit(1); }
    }

    private void run(String[] args) throws Exception {
        String socketName = "marionette";
        int maxSize = 1280, bitrate = 4000000, fps = 30;
        for (int i = 0; i < args.length; i += 2) {
            if (i + 1 >= args.length) throw new IllegalArgumentException("Missing value for " + args[i]);
            if ("--socket".equals(args[i])) socketName = args[i + 1];
            else if ("--max-size".equals(args[i])) maxSize = bounded(args[i + 1], 64, 8192, args[i]);
            else if ("--bit-rate".equals(args[i])) bitrate = bounded(args[i + 1], 100000, 100000000, args[i]);
            else if ("--max-fps".equals(args[i])) fps = bounded(args[i + 1], 1, 120, args[i]);
            else throw new IllegalArgumentException("Unknown option " + args[i]);
        }
        if (!socketName.matches("[A-Za-z0-9_.-]{1,90}")) throw new IllegalArgumentException("Invalid abstract socket name");
        if (Looper.myLooper() == null) Looper.prepareMainLooper();
        server = new LocalServerSocket(socketName);
        hub = new VideoHub(new Runnable() { public void run() { if (encoder != null) encoder.requestKeyframe(); } });
        Runtime.getRuntime().addShutdownHook(new Thread(new Runnable() { public void run() { close(); } }, "marionette-shutdown"));
        long started = SystemClock.uptimeMillis();
        String stage = "platform";
        try {
            if (android.os.Build.VERSION.SDK_INT < 30 || android.os.Build.VERSION.SDK_INT > 34)
                throw new UnsupportedOperationException("Target Android APIs are 30–34; detected " + android.os.Build.VERSION.SDK_INT);
            platform = new Platform();
            stage = "encoder";
            encoder = new Encoder(platform, maxSize, bitrate, fps);
            encoder.hub = hub;
            controls = new Controls(platform, encoder);
            encoderThread = new Thread(encoder, "marionette-encoder");
            encoderThread.start();
        } catch (Exception e) {
            startupFailure = "Device initialization failed at " + stage + " (API " + android.os.Build.VERSION.SDK_INT
                    + ", " + (SystemClock.uptimeMillis() - started) + " ms): " + message(e);
            diagnostic("startup-" + stage, started, e);
            System.err.println(startupFailure); hub.error(startupFailure);
        }
        // Android services may post work to the main looper; socket accept must not occupy it.
        new Thread(new Runnable() { public void run() { accept(); } }, "marionette-accept").start();
        System.err.println("Marionette listening on localabstract:" + socketName);
        Looper.loop();
    }

    private void accept() {
        try {
            while (running) {
                final LocalSocket socket = server.accept();
                if (!connections.tryAcquire()) { socket.close(); continue; }
                sockets.add(socket);
                new Thread(new Runnable() { public void run() { connection(socket); } }, "marionette-connection").start();
            }
        } catch (Exception e) {
            if (running) { System.err.println("Socket accept failed: " + message(e)); close(); System.exit(1); }
        }
    }

    private void connection(LocalSocket socket) {
        boolean video = false;
        boolean input = false;
        try {
            socket.setSoTimeout(5000);
            int channel = socket.getInputStream().read();
            if (channel == 1) {
                socket.setSoTimeout(5000); // Android LocalSocket applies send and receive timeouts.
                hub.add(socket); video = true;
            } else if (channel == 2 || channel == 3 || channel == 4 || channel == 5) {
                input = channel == 3 || channel == 4;
                socket.setSoTimeout(channel == 5 ? 5000 : 0);
                InputStream in = new BufferedInputStream(socket.getInputStream(), 8192); OutputStream out = socket.getOutputStream();
                while (running) {
                    String line = readLine(in);
                    if (line == null) return;
                    JSONObject response;
                    Object requestID = JSONObject.NULL;
                    try {
                        JSONObject request = new JSONObject(line);
                        requestID = request.has("id") ? request.opt("id") : JSONObject.NULL;
                        if (controls == null) response = new JSONObject().put("id", request.has("id") ? request.opt("id") : JSONObject.NULL)
                                .put("ok", false).put("error", startupFailure);
                        else if (channel == 5) {
                            if (!"resetInput".equals(request.optString("action")))
                                throw new IllegalArgumentException("Unsupported cleanup action");
                            controls.resetLease(request.getLong("leaseEpoch"));
                            response = new JSONObject().put("id", requestID).put("ok", true);
                        } else if (input) {
                            String action = request.optString("action", "");
                            if (!("hover".equals(action) || "touch".equals(action) || "tap".equals(action)
                                    || "swipe".equals(action) || "key".equals(action) || "text".equals(action)))
                                throw new IllegalArgumentException("Unsupported realtime input action");
                            response = controls.execute(request, socket);
                        } else {
                            if ("warmup".equals(request.optString("action"))) {
                                long warmupStarted = SystemClock.uptimeMillis();
                                try { platform.requireActiveRoot(1500).recycle(); }
                                catch (Exception e) { diagnostic("accessibility-warmup-failed", warmupStarted, e); throw e; }
                                diagnostic("accessibility-root-ready", warmupStarted, null);
                            }
                            response = controls.execute(request);
                        }
                    } catch (Exception e) {
                        response = new JSONObject().put("id", requestID).put("ok", false).put("error", message(e));
                    }
                    byte[] bytes = (response.toString() + "\n").getBytes(StandardCharsets.UTF_8);
                    if (bytes.length > MAX_LINE) throw new IllegalStateException("Control response exceeds 1 MiB");
                    out.write(bytes); out.flush();
                    if (channel == 5) return;
                }
            } else throw new IllegalArgumentException("Unknown channel byte: " + channel);
        } catch (Exception e) {
            if (running) System.err.println("Connection closed: " + message(e));
        } finally {
            if (input && controls != null) controls.resetInput(socket);
            sockets.remove(socket); connections.release();
            if (!video) try { socket.close(); } catch (Exception ignored) { }
        }
    }

    private static String readLine(InputStream in) throws Exception {
        ByteArrayOutputStream line = new ByteArrayOutputStream();
        while (true) {
            int b = in.read();
            if (b == -1) {
                if (line.size() != 0) throw new IllegalArgumentException("Truncated control JSON line");
                return null;
            }
            if (b == '\n') return StandardCharsets.UTF_8.newDecoder()
                    .onMalformedInput(CodingErrorAction.REPORT).onUnmappableCharacter(CodingErrorAction.REPORT)
                    .decode(ByteBuffer.wrap(line.toByteArray())).toString();
            if (line.size() >= MAX_LINE - 1) throw new IllegalArgumentException("Control request exceeds 1 MiB");
            line.write(b);
        }
    }

    private synchronized void close() {
        if (!running) return;
        running = false;
        if (encoder != null) encoder.stop();
        if (server != null) try { server.close(); } catch (Exception ignored) { }
        synchronized (sockets) { for (LocalSocket socket : sockets) try { socket.close(); } catch (Exception ignored) { } }
        if (hub != null) hub.close();
        if (encoderThread != null) try { encoderThread.join(2000); } catch (InterruptedException e) { Thread.currentThread().interrupt(); }
        if (platform != null) platform.close();
    }

    static String message(Throwable error) {
        while (error.getCause() != null && error.getCause() != error) error = error.getCause();
        String text = error.getMessage();
        return error.getClass().getSimpleName() + (text == null ? "" : ": " + text);
    }
    static void diagnostic(String stage, long started, Throwable error) {
        StringBuilder line = new StringBuilder("Marionette ").append(stage)
                .append(" api=").append(android.os.Build.VERSION.SDK_INT)
                .append(" elapsedMs=").append(SystemClock.uptimeMillis() - started);
        // Error text may contain input values. Log only exception types and our code locations.
        for (int causes = 0; error != null && causes < 4; causes++, error = error.getCause()) {
            line.append(" error=").append(error.getClass().getSimpleName());
            for (StackTraceElement frame : error.getStackTrace()) {
                if (frame.getClassName().startsWith("dev.marionette.")) {
                    line.append(" at=").append(frame.getFileName()).append(':').append(frame.getLineNumber());
                    break;
                }
            }
        }
        System.err.println(line.toString());
    }
    private static int bounded(String raw, int min, int max, String name) {
        int value = Integer.parseInt(raw);
        if (value < min || value > max) throw new IllegalArgumentException(name + " must be in " + min + ".." + max);
        return value;
    }
}
