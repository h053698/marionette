package dev.marionette;

import android.os.Bundle;
import android.os.SystemClock;
import android.view.InputDevice;
import android.view.KeyEvent;
import android.view.MotionEvent;
import android.view.accessibility.AccessibilityNodeInfo;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.locks.ReentrantReadWriteLock;
import org.json.JSONObject;

final class Controls {
    private final Platform platform;
    private final Encoder encoder;
    private final Elements elements;
    private final Apps apps;
    private final Processes processes;
    private boolean hovering;
    private final Object inputLock = new Object(), textLock = new Object();
    private final ReentrantReadWriteLock leaseGate = new ReentrantReadWriteLock(true);
    private final AtomicLong revokedEpoch = new AtomicLong();
    private Object touchOwner, hoverOwner;
    private long touchEpoch, hoverEpoch;
    private long touchDown;
    private float touchX, touchY;
    Controls(Platform platform, Encoder encoder) {
        this.platform = platform; this.encoder = encoder; this.elements = new Elements(platform); this.apps = new Apps(platform);
        this.processes = new Processes(platform);
    }

    JSONObject execute(JSONObject request) { return execute(request, null); }

    /** Locks the display rotation (0..3) or hands control back to the sensor ("free"). */
    private JSONObject rotate(JSONObject request) throws Exception {
        String mode;
        if (request.opt("rotation") instanceof String && "free".equals(request.getString("rotation"))) mode = "free";
        else mode = "lock " + integer(request, "rotation", 0, 3);
        String[] words = mode.split(" ");
        String[] command = new String[3 + words.length];
        command[0] = "/system/bin/cmd"; command[1] = "window"; command[2] = "user-rotation";
        System.arraycopy(words, 0, command, 3, words.length);
        final java.lang.Process process = new ProcessBuilder(command).redirectErrorStream(true).start();
        Thread watchdog = new Thread(new Runnable() {
            public void run() {
                try { Thread.sleep(3000); process.destroy(); }
                catch (InterruptedException ignored) { Thread.currentThread().interrupt(); }
            }
        }, "marionette-rotate");
        watchdog.setDaemon(true);
        watchdog.start();
        try {
            java.io.InputStream out = process.getInputStream();
            byte[] buffer = new byte[512];
            int length = out.read(buffer);
            int code = process.waitFor();
            if (code != 0) throw new IllegalStateException("Rotation change failed: "
                    + (length > 0 ? new String(buffer, 0, length, "UTF-8").trim() : "exit " + code));
        } finally { watchdog.interrupt(); process.destroy(); }
        return new JSONObject().put("rotation", request.get("rotation"));
    }


    JSONObject execute(JSONObject request, Object owner) {
        String action = request.optString("action", "");
        Object lock = "text".equals(action) ? textLock
                : ("hover".equals(action) || "touch".equals(action) || "tap".equals(action)
                || "swipe".equals(action) || "key".equals(action)) ? inputLock : this;
        long epoch = request.optLong("leaseEpoch", 0);
        if (epoch == 0) {
            synchronized (lock) { return executeLocked(request, owner, epoch); }
        }
        leaseGate.readLock().lock();
        try {
            synchronized (lock) { return executeLocked(request, owner, epoch); }
        } finally { leaseGate.readLock().unlock(); }
    }

    // Slow package/process/tree queries do not hold the native input lock.
    private JSONObject executeLocked(JSONObject request, Object owner, long epoch) {
        JSONObject response = new JSONObject();
        try {
            if (!(request.opt("id") instanceof String)) throw new IllegalArgumentException("id must be a string");
            if (!(request.opt("action") instanceof String)) throw new IllegalArgumentException("action must be a string");
            String id = request.getString("id");
            response.put("id", id);
            String action = request.getString("action");
            checkLease(epoch);
            Object result = JSONObject.NULL;
            if ("warmup".equals(action)) {
                platform.automation();
                result = new JSONObject().put("automation", "UiAutomation").put("ready", true);
            } else if ("info".equals(action)) {
                if (encoder.failure != null) throw new IllegalStateException(encoder.failure);
                JSONObject value = encoder.metadata;
                if (value == null) throw new IllegalStateException("Encoder metadata not ready");
                result = new JSONObject(value.toString()).put("sdk", android.os.Build.VERSION.SDK_INT)
                        .put("model", android.os.Build.MODEL)
                        .put("automation", "UiAutomation").put("automationReady", platform.automationReady())
                        .put("capabilities", new org.json.JSONArray(new String[]{"tap", "swipe", "touch", "hover", "key", "text", "uiTree", "findElement", "click", "apps", "appIcon", "processes", "launchApp", "rotate"}));
            } else if ("keyframe".equals(action)) {
                if (encoder.failure != null) throw new IllegalStateException(encoder.failure);
                encoder.requestKeyframe();
            } else if ("videoProfile".equals(action)) {
                if (!(request.opt("profile") instanceof String)) throw new IllegalArgumentException("profile must be a string");
                encoder.videoProfile(request.getString("profile"));
            } else if ("hover".equals(action)) {
                Platform.Geometry g = platform.geometry();
                float x = coordinate(request, "x", g.width), y = coordinate(request, "y", g.height);
                String phase = request.optString("phase", "move");
                if (!"move".equals(phase) && !"exit".equals(phase)) throw new IllegalArgumentException("hover phase must be move or exit");
                if (touchOwner != null) {
                    // A late mouse sample from the independent hover socket
                    // must not cancel an in-progress touchscreen sequence.
                } else if ("exit".equals(phase)) {
                    if (hovering) mouse(MotionEvent.ACTION_HOVER_EXIT, x, y);
                    hovering = false;
                    hoverOwner = null;
                } else {
                    if (!hovering) { mouse(MotionEvent.ACTION_HOVER_ENTER, x, y); hovering = true; }
                    hoverOwner = owner; hoverEpoch = epoch;
                    mouse(MotionEvent.ACTION_HOVER_MOVE, x, y);
                }
            } else if ("touch".equals(action)) {
                if (owner == null) throw new IllegalArgumentException("touch requires a realtime input connection");
                Platform.Geometry g = platform.geometry();
                float x = coordinate(request, "x", g.width), y = coordinate(request, "y", g.height);
                String phase = request.getString("phase");
                if ("down".equals(phase)) {
                    cancelTouch();
                    if (hovering) mouse(MotionEvent.ACTION_HOVER_EXIT, x, y);
                    hovering = false; hoverOwner = null;
                    touchDown = SystemClock.uptimeMillis(); touchX = x; touchY = y;
                    touchOwner = owner; touchEpoch = epoch;
                    touch(touchDown, MotionEvent.ACTION_DOWN, x, y);
                } else if ("move".equals(phase) || "up".equals(phase) || "cancel".equals(phase)) {
                    if (touchOwner != owner) throw new IllegalStateException("Touch sequence is no longer active");
                    touchX = x; touchY = y;
                    touch(touchDown, "move".equals(phase) ? MotionEvent.ACTION_MOVE
                            : "up".equals(phase) ? MotionEvent.ACTION_UP : MotionEvent.ACTION_CANCEL, x, y);
                    if (!"move".equals(phase)) touchOwner = null;
                } else throw new IllegalArgumentException("touch phase must be down, move, up or cancel");
            } else if ("tap".equals(action)) {
                Platform.Geometry g = platform.geometry();
                float x = coordinate(request, "x", g.width), y = coordinate(request, "y", g.height);
                gesture(x, y, x, y, 0, epoch);
            } else if ("swipe".equals(action)) {
                Platform.Geometry g = platform.geometry();
                int duration = integer(request, "durationMs", 50, 5000);
                gesture(coordinate(request, "x1", g.width), coordinate(request, "y1", g.height),
                        coordinate(request, "x2", g.width), coordinate(request, "y2", g.height), duration, epoch);
            } else if ("key".equals(action)) {
                int code = integer(request, "keyCode", 0, KeyEvent.getMaxKeyCode());
                long now = SystemClock.uptimeMillis();
                boolean down = false;
                try {
                    platform.inject(new KeyEvent(now, now, KeyEvent.ACTION_DOWN, code, 0,
                            0, android.view.KeyCharacterMap.VIRTUAL_KEYBOARD, 0, 0, InputDevice.SOURCE_KEYBOARD));
                    down = true;
                } finally {
                    if (down) platform.inject(new KeyEvent(now, SystemClock.uptimeMillis(), KeyEvent.ACTION_UP,
                            code, 0, 0, android.view.KeyCharacterMap.VIRTUAL_KEYBOARD, 0, 0, InputDevice.SOURCE_KEYBOARD));
                }
            } else if ("text".equals(action)) {
                if (!(request.opt("text") instanceof String)) throw new IllegalArgumentException("text must be a string");
                if (request.has("selector")) result = elements.setText(request);
                else setText(request.getString("text"));
            } else if ("rotate".equals(action)) {
                result = rotate(request);
            } else if ("apps".equals(action)) {
                result = apps.list();
            } else if ("appIcon".equals(action)) {
                if (!(request.opt("packageName") instanceof String)) throw new IllegalArgumentException("packageName must be a string");
                result = apps.icon(request.getString("packageName"));
            } else if ("processes".equals(action)) {
                result = processes.list();
            } else if ("launchApp".equals(action)) {
                if (!(request.opt("packageName") instanceof String)) throw new IllegalArgumentException("packageName must be a string");
                result = apps.launch(request.getString("packageName"));
            } else if ("uiTree".equals(action)) {
                result = elements.tree(request);
            } else if ("findElement".equals(action) || "click".equals(action)) {
                AccessibilityNodeInfo node = elements.find(request);
                try {
                    result = Elements.describe(node);
                    if ("click".equals(action)) {
                        checkLease(epoch);
                        if (!node.isEnabled()) throw new IllegalStateException("Element is disabled");
                        if (!node.performAction(AccessibilityNodeInfo.ACTION_CLICK)) {
                            android.graphics.Rect bounds = new android.graphics.Rect();
                            node.getBoundsInScreen(bounds);
                            Platform.Geometry g = platform.geometry();
                            if (!bounds.intersect(0, 0, g.width, g.height) || bounds.isEmpty())
                                throw new IllegalStateException("Element has no visible bounds");
                            synchronized (inputLock) { gesture(bounds.exactCenterX(), bounds.exactCenterY(), bounds.exactCenterX(), bounds.exactCenterY(), 0, epoch); }
                        }
                    }
                } finally { node.recycle(); }
            } else throw new IllegalArgumentException("Unknown action: " + action);
            response.put("ok", true).put("result", result);
        } catch (Exception e) {
            try {
                if (!response.has("id")) response.put("id", request.opt("id") == null ? JSONObject.NULL : request.opt("id"));
                response.put("ok", false).put("error", Main.message(e));
            } catch (Exception ignored) { }
        }
        return response;
    }

    void resetLease(long epoch) throws Exception {
        if (epoch <= 0) throw new IllegalArgumentException("Invalid lease epoch");
        // Signal running gestures before waiting for all accepted mutations.
        long prior;
        do {
            prior = revokedEpoch.get();
        } while (epoch > prior && !revokedEpoch.compareAndSet(prior, epoch));
        leaseGate.writeLock().lock();
        try {
            synchronized (inputLock) {
                if (touchOwner != null && touchEpoch <= epoch) cancelTouch();
                if (hovering && hoverEpoch <= epoch) {
                    mouse(MotionEvent.ACTION_HOVER_EXIT, touchX, touchY);
                    hovering = false; hoverOwner = null;
                }
            }
        } finally { leaseGate.writeLock().unlock(); }
    }

    private void checkLease(long epoch) {
        if (epoch != 0 && epoch <= revokedEpoch.get()) throw new IllegalStateException("Input lease ended");
    }

    void resetInput(Object owner) {
        synchronized (inputLock) {
            if (touchOwner == owner) try { cancelTouch(); } catch (Exception ignored) { }
            if (hovering && hoverOwner == owner) {
                try {
                    mouse(MotionEvent.ACTION_HOVER_EXIT, touchX, touchY);
                    hovering = false; hoverOwner = null;
                } catch (Exception ignored) { /* The lease cleanup will retry. */ }
            }
        }
    }

    private void cancelTouch() throws Exception {
        if (touchOwner == null) return;
        touch(touchDown, MotionEvent.ACTION_CANCEL, touchX, touchY);
        touchOwner = null;
    }

    private static int integer(JSONObject request, String field, int min, int max) throws Exception {
        Object raw = request.get(field);
        if (!(raw instanceof Number)) throw new IllegalArgumentException(field + " must be an integer");
        double number = ((Number) raw).doubleValue();
        if (Double.isNaN(number) || number < min || number > max || number != Math.rint(number))
            throw new IllegalArgumentException(field + " must be an integer in " + min + ".." + max);
        return (int) number;
    }

    private static float coordinate(JSONObject request, String field, int bound) throws Exception {
        Object raw = request.get(field);
        if (!(raw instanceof Number)) throw new IllegalArgumentException(field + " must be numeric");
        double value = ((Number) raw).doubleValue();
        if (Double.isNaN(value) || Double.isInfinite(value) || value < 0 || value >= bound)
            throw new IllegalArgumentException(field + " outside display bounds 0.." + (bound - 1));
        return (float) value;
    }

    private void gesture(float x1, float y1, float x2, float y2, int duration, long epoch) throws Exception {
        checkLease(epoch);
        cancelTouch();
        if (hovering) mouse(MotionEvent.ACTION_HOVER_EXIT, x1, y1);
        hovering = false; hoverOwner = null;
        touchDown = SystemClock.uptimeMillis(); touchX = x1; touchY = y1;
        touchOwner = this; touchEpoch = epoch;
        try {
            touch(touchDown, MotionEvent.ACTION_DOWN, touchX, touchY);
            if (duration > 0) {
                long end = touchDown + duration;
                while (SystemClock.uptimeMillis() < end) {
                    Thread.sleep(Math.min(16, Math.max(1, end - SystemClock.uptimeMillis())));
                    checkLease(epoch);
                    float progress = Math.min(1f, (float) (SystemClock.uptimeMillis() - touchDown) / duration);
                    touchX = x1 + (x2 - x1) * progress; touchY = y1 + (y2 - y1) * progress;
                    touch(touchDown, MotionEvent.ACTION_MOVE, touchX, touchY);
                }
            }
            checkLease(epoch);
            touchX = x2; touchY = y2;
            touch(touchDown, MotionEvent.ACTION_UP, touchX, touchY);
            touchOwner = null;
        } finally {
            // Preserve ownership on a failed cancel so the lease barrier can retry.
            cancelTouch();
        }
    }

    private void touch(long down, int action, float x, float y) throws Exception {
        MotionEvent.PointerProperties properties = new MotionEvent.PointerProperties();
        properties.id = 0; properties.toolType = MotionEvent.TOOL_TYPE_FINGER;
        MotionEvent.PointerCoords coordinates = new MotionEvent.PointerCoords();
        coordinates.x = x; coordinates.y = y; coordinates.pressure = action == MotionEvent.ACTION_UP ? 0f : 1f;
        coordinates.size = 1f;
        MotionEvent event = MotionEvent.obtain(down, SystemClock.uptimeMillis(), action, 1,
                new MotionEvent.PointerProperties[] { properties }, new MotionEvent.PointerCoords[] { coordinates },
                0, 0, 1f, 1f, 0, 0, InputDevice.SOURCE_TOUCHSCREEN, 0);
        try { platform.inject(event); } finally { event.recycle(); }
    }

    private void mouse(int action, float x, float y) throws Exception {
        long now = SystemClock.uptimeMillis();
        MotionEvent.PointerProperties properties = new MotionEvent.PointerProperties();
        properties.id = 0;
        properties.toolType = MotionEvent.TOOL_TYPE_MOUSE;
        MotionEvent.PointerCoords coordinates = new MotionEvent.PointerCoords();
        coordinates.x = x; coordinates.y = y;
        MotionEvent event = MotionEvent.obtain(now, now, action, 1,
                new MotionEvent.PointerProperties[]{properties}, new MotionEvent.PointerCoords[]{coordinates},
                0, 0, 1, 1, 0, 0, InputDevice.SOURCE_MOUSE, 0);
        try { platform.inject(event); } finally { event.recycle(); }
    }

    private void setText(String text) throws Exception {
        android.app.UiAutomation automation = platform.automation();
        AccessibilityNodeInfo root = automation.getRootInActiveWindow();
        // A freshly connected accessibility service can precede its first window event.
        // Bound the wait so unsupported windows still return an explicit error.
        long deadline = SystemClock.uptimeMillis() + 1500;
        while (root == null && SystemClock.uptimeMillis() < deadline) {
            Thread.sleep(50);
            root = automation.getRootInActiveWindow();
        }
        if (root == null) throw new UnsupportedOperationException("No accessible active window for Unicode text entry");
        AccessibilityNodeInfo focused = null;
        try {
            focused = root.findFocus(AccessibilityNodeInfo.FOCUS_INPUT);
            if (focused == null || !focused.isEditable())
                throw new UnsupportedOperationException("No focused editable accessibility node; text entry unsupported for this field");
            Bundle args = new Bundle();
            args.putCharSequence(AccessibilityNodeInfo.ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE, text);
            if (!focused.performAction(AccessibilityNodeInfo.ACTION_SET_TEXT, args))
                throw new UnsupportedOperationException("Focused field rejected accessibility ACTION_SET_TEXT");
        } finally {
            if (focused != null && focused != root) focused.recycle();
            root.recycle();
        }
    }
}
