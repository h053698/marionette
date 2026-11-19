package dev.marionette;

import android.app.UiAutomation;
import android.content.Context;
import android.content.Intent;
import android.graphics.Point;
import android.graphics.Rect;
import android.hardware.display.DisplayManager;
import android.hardware.display.VirtualDisplay;
import android.os.HandlerThread;
import android.os.IBinder;
import android.os.SystemClock;
import android.view.Display;
import android.view.InputEvent;
import android.view.Surface;
import android.view.accessibility.AccessibilityNodeInfo;
import java.lang.reflect.Method;

/** All non-SDK framework entry points are kept here. No root or external runner. */
final class Platform {
    final Context context;
    final DisplayManager displays;
    private Object inputManager;
    private Method inject;
    private UiAutomation automation;
    private HandlerThread automationThread;
    private final Object windowEvents = new Object();
    private long windowEventSequence;
    private volatile boolean activeRootReady;
    volatile String captureMethod;

    Platform() throws Exception {
        Class<?> activityThread = Class.forName("android.app.ActivityThread");
        Object thread = activityThread.getMethod("systemMain").invoke(null);
        Context system = (Context) activityThread.getMethod("getSystemContext").invoke(thread);
        // createPackageContext dereferences this hidden service without a null check.
        // Report service availability explicitly instead of an opaque framework NPE.
        if (activityThread.getMethod("getPackageManager").invoke(null) == null)
            throw new IllegalStateException("Android package service unavailable while creating shell context");
        context = system.createPackageContext("com.android.shell", 0);
        displays = (DisplayManager) context.getSystemService(Context.DISPLAY_SERVICE);
        if (displays == null) throw new IllegalStateException("DisplayManager unavailable for shell context");
    }

    Geometry geometry() {
        Display d = displays.getDisplay(Display.DEFAULT_DISPLAY);
        if (d == null) throw new IllegalStateException("Default display unavailable");
        Point size = new Point();
        d.getRealSize(size);
        android.util.DisplayMetrics metrics = new android.util.DisplayMetrics();
        d.getRealMetrics(metrics);
        return new Geometry(size.x, size.y, d.getRotation(), metrics.densityDpi);
    }

    interface Mirror { void close(); }

    Mirror mirror(Surface surface, Geometry g, int width, int height) throws Exception {
        if (android.os.Build.VERSION.SDK_INT <= 33) {
            try { Mirror mirror = legacyMirror(surface, g, width, height); captureMethod = "SurfaceControl"; return mirror; }
            catch (Exception e) { System.err.println("SurfaceControl mirror unavailable: " + Main.message(e)); }
        }
        try {
            final VirtualDisplay vd = displays.createVirtualDisplay("Marionette", width, height,
                    g.density, surface, DisplayManager.VIRTUAL_DISPLAY_FLAG_PUBLIC
                    | DisplayManager.VIRTUAL_DISPLAY_FLAG_AUTO_MIRROR);
            if (vd == null) throw new IllegalStateException("createVirtualDisplay returned null");
            captureMethod = "DisplayManager.VirtualDisplay";
            return new Mirror() { public void close() { vd.release(); } };
        } catch (Exception e) {
            throw new IllegalStateException("DisplayManager shell mirroring unsupported on API "
                    + android.os.Build.VERSION.SDK_INT + ": " + Main.message(e), e);
        }
    }

    private Mirror legacyMirror(Surface surface, Geometry g, int width, int height) throws Exception {
        final Class<?> sc = Class.forName("android.view.SurfaceControl");
        final IBinder token = (IBinder) sc.getMethod("createDisplay", String.class, boolean.class)
                .invoke(null, "Marionette", false);
        boolean success = false;
        try {
            sc.getMethod("openTransaction").invoke(null);
            try {
                sc.getMethod("setDisplaySurface", IBinder.class, Surface.class).invoke(null, token, surface);
                sc.getMethod("setDisplayProjection", IBinder.class, int.class, Rect.class, Rect.class)
                        .invoke(null, token, 0, new Rect(0, 0, g.width, g.height), new Rect(0, 0, width, height));
                // The default physical display's layer stack is normally zero. Read it explicitly.
                Method getLayerStack = Display.class.getDeclaredMethod("getLayerStack");
                getLayerStack.setAccessible(true);
                int stack = (Integer) getLayerStack.invoke(displays.getDisplay(Display.DEFAULT_DISPLAY));
                sc.getMethod("setDisplayLayerStack", IBinder.class, int.class).invoke(null, token, stack);
            } finally { sc.getMethod("closeTransaction").invoke(null); }
            success = true;
            return new Mirror() { public void close() {
                try { sc.getMethod("destroyDisplay", IBinder.class).invoke(null, token); }
                catch (Exception e) { System.err.println("Display cleanup: " + Main.message(e)); }
            }};
        } finally {
            if (!success) sc.getMethod("destroyDisplay", IBinder.class).invoke(null, token);
        }
    }

    synchronized void inject(InputEvent event) throws Exception {
        if (inputManager == null) {
            Class<?> type = Class.forName(android.os.Build.VERSION.SDK_INT >= 34
                    ? "android.hardware.input.InputManagerGlobal" : "android.hardware.input.InputManager");
            inputManager = type.getMethod("getInstance").invoke(null);
            inject = type.getMethod("injectInputEvent", InputEvent.class, int.class);
        }
        if (!Boolean.TRUE.equals(inject.invoke(inputManager, event, 2)))
            throw new IllegalStateException("Input injection rejected by Android");
    }

    void launch(Intent intent) throws Exception {
        // app_process has no registered application thread. Pass a null caller
        // and the shell package explicitly instead of Context.startActivity's
        // synthetic system ActivityThread, which OEMs may attribute to uid 1000.
        boolean modern = android.os.Build.VERSION.SDK_INT >= 29;
        Object service = Class.forName(modern ? "android.app.ActivityTaskManager" : "android.app.ActivityManager")
                .getMethod("getService").invoke(null);
        Class<?> type = Class.forName(modern ? "android.app.IActivityTaskManager" : "android.app.IActivityManager");
        for (Method method : type.getMethods()) {
            if (!method.getName().equals("startActivityAsUser")) continue;
            Class<?>[] parameters = method.getParameterTypes();
            boolean feature = parameters.length == 12 && parameters[3] == Intent.class;
            boolean legacy = parameters.length == 11 && parameters[2] == Intent.class;
            if (!feature && !legacy) continue;
            Object[] args = feature
                    ? new Object[] { null, "com.android.shell", null, intent, null, null, null, 0, 0, null, null, -2 }
                    : new Object[] { null, "com.android.shell", intent, null, null, null, 0, 0, null, null, -2 };
            int result = (Integer) method.invoke(service, args);
            if (result < 0) throw new IllegalStateException("Android rejected app launch: " + result);
            return;
        }
        throw new IllegalStateException("Native activity launch unsupported on API " + android.os.Build.VERSION.SDK_INT);
    }

    synchronized UiAutomation automation() throws Exception {
        if (automation != null) return automation;
        automationThread = new HandlerThread("marionette-accessibility");
        automationThread.start();
        UiAutomation candidate = null;
        long started = SystemClock.uptimeMillis();
        try {
            Class<?> connectionType = Class.forName("android.app.IUiAutomationConnection");
            Object connection = Class.forName("android.app.UiAutomationConnection").getConstructor().newInstance();
            candidate = (UiAutomation) UiAutomation.class
                    .getConstructor(android.os.Looper.class, connectionType)
                    .newInstance(automationThread.getLooper(), connection);
            // Keep existing accessibility services running (FLAG_DONT_SUPPRESS_ACCESSIBILITY_SERVICES).
            UiAutomation.class.getMethod("connect", int.class).invoke(candidate, 1);
            android.accessibilityservice.AccessibilityServiceInfo info = candidate.getServiceInfo();
            if (info == null) throw new IllegalStateException("Accessibility service configuration unavailable after attachment");
            info.flags |= android.accessibilityservice.AccessibilityServiceInfo.FLAG_REPORT_VIEW_IDS;
            candidate.setServiceInfo(info);
            candidate.setOnAccessibilityEventListener(new UiAutomation.OnAccessibilityEventListener() {
                public void onAccessibilityEvent(android.view.accessibility.AccessibilityEvent event) {
                    synchronized (windowEvents) { windowEventSequence++; windowEvents.notifyAll(); }
                }
            });
            // Publish only a fully configured connection. A failed setup must be retryable.
            automation = candidate;
            Main.diagnostic("accessibility-attached", started, null);
            return automation;
        } catch (Exception e) {
            if (candidate != null) try { UiAutomation.class.getMethod("disconnect").invoke(candidate); } catch (Exception ignored) { }
            automationThread.quitSafely();
            automationThread = null;
            activeRootReady = false;
            Main.diagnostic("accessibility-attachment-failed", started, e);
            throw new IllegalStateException("Shell accessibility attachment unsupported: " + Main.message(e), e);
        }
    }

    AccessibilityNodeInfo activeRoot(long waitMs) throws Exception {
        UiAutomation current = automation();
        long started = SystemClock.uptimeMillis(), deadline = started + Math.max(0, waitMs);
        int reads = 0;
        while (true) {
            long sequence;
            synchronized (windowEvents) { sequence = windowEventSequence; }
            // Android bounds each interaction internally; an in-flight Binder read cannot
            // be interrupted by our deadline. Never begin another read after it expires.
            activeRootReady = false;
            AccessibilityNodeInfo root;
            try { root = current.getRootInActiveWindow(); }
            catch (Exception e) { Main.diagnostic("accessibility-root-read-failed", started, e); throw e; }
            reads++;
            activeRootReady = root != null;
            if (root != null) return root;
            synchronized (windowEvents) {
                long remaining;
                while (windowEventSequence == sequence && (remaining = deadline - SystemClock.uptimeMillis()) > 0)
                    windowEvents.wait(remaining);
            }
            if (SystemClock.uptimeMillis() >= deadline) {
                System.err.println("Marionette accessibility-root-unavailable api=" + android.os.Build.VERSION.SDK_INT
                        + " elapsedMs=" + (SystemClock.uptimeMillis() - started) + " reads=" + reads);
                return null;
            }
        }
    }

    AccessibilityNodeInfo requireActiveRoot(long waitMs) throws Exception {
        AccessibilityNodeInfo root = activeRoot(waitMs);
        if (root == null)
            throw new IllegalStateException("No accessible active window; accessibility attached but UI root is not ready");
        return root;
    }

    synchronized boolean automationReady() { return automation != null && activeRootReady; }

    synchronized void close() {
        activeRootReady = false;
        if (automation != null) {
            try { UiAutomation.class.getMethod("disconnect").invoke(automation); } catch (Exception ignored) { }
            automation = null;
        }
        if (automationThread != null) { automationThread.quitSafely(); automationThread = null; }
    }

    static final class Geometry {
        final int width, height, rotation, density;
        Geometry(int w, int h, int r, int d) { width = w; height = h; rotation = r; density = d; }
        boolean same(Geometry other) { return width == other.width && height == other.height
                && rotation == other.rotation && density == other.density; }
    }
}
