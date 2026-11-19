package dev.marionette;

import android.graphics.Rect;
import android.os.Bundle;
import android.os.SystemClock;
import android.view.accessibility.AccessibilityNodeInfo;
import android.util.Xml;
import java.io.StringWriter;
import java.util.Iterator;
import org.json.JSONArray;
import org.json.JSONObject;
import org.xmlpull.v1.XmlSerializer;

/** Accessibility queries execute inside the persistent runner, without adb shell/UI dump subprocesses. */
final class Elements {
    private final Platform platform;
    private static final int MAX_NODES = 2000, MAX_DEPTH = 64;
    private static final long TRAVERSAL_MS = 1500;
    Elements(Platform platform) { this.platform = platform; }

    AccessibilityNodeInfo find(JSONObject request) throws Exception {
        JSONObject selector = request.getJSONObject("selector");
        boolean predicate = false;
        for (Iterator<String> it = selector.keys(); it.hasNext();) {
            String key = it.next();
            if ("index".equals(key)) continue;
            if (!"resourceId".equals(key) && !"text".equals(key) && !"description".equals(key) && !"className".equals(key))
                throw new IllegalArgumentException("Unknown selector field: " + key);
            if (!(selector.get(key) instanceof String)) throw new IllegalArgumentException(key + " must be a string");
            predicate = true;
        }
        if (!predicate) throw new IllegalArgumentException("Selector needs resourceId, text, description or className");
        int index = number(selector, "index", 0, 0, MAX_NODES - 1);
        int wait = number(request, "waitMs", 0, 0, 8000);
        // Attachment is a separate startup operation; traversal budgets start afterwards.
        platform.automation();
        long started = SystemClock.uptimeMillis();
        long end = started + wait;
        boolean sawRoot = false;
        do {
            long readDeadline = SystemClock.uptimeMillis() + TRAVERSAL_MS;
            Budget budget = new Budget(readDeadline);
            AccessibilityNodeInfo root = platform.activeRoot(wait == 0 ? 0 : Math.min(TRAVERSAL_MS, Math.max(0, end - SystemClock.uptimeMillis())));
            if (root != null) {
                sawRoot = true;
                try {
                    // Android resolves resource IDs directly; avoid dumping the entire tree for find+click.
                    if (selector.has("resourceId")) {
                        if (!budget.available(0)) throw incomplete(budget, started);
                        java.util.List<AccessibilityNodeInfo> nodes = root.findAccessibilityNodeInfosByViewId(selector.getString("resourceId"));
                        AccessibilityNodeInfo found = null;
                        try {
                            int matched = 0;
                            for (AccessibilityNodeInfo node : nodes) {
                                if (!budget.enter(0)) break;
                                if (matches(node, selector) && matched++ == index) {
                                    found = AccessibilityNodeInfo.obtain(node); break;
                                }
                            }
                        } finally { for (AccessibilityNodeInfo node : nodes) node.recycle(); }
                        if (found != null) return found;
                    } else {
                        AccessibilityNodeInfo found = search(root, selector, new int[]{index}, budget, 0);
                        if (found != null) return found;
                    }
                    if (budget.reason != null) throw incomplete(budget, started);
                } finally { root.recycle(); }
            }
            if (SystemClock.uptimeMillis() >= end) break;
            Thread.sleep(Math.min(50, Math.max(1, end - SystemClock.uptimeMillis())));
        } while (true);
        if (!sawRoot)
            throw new IllegalStateException("No accessible active window; accessibility attached but UI root is not ready");
        throw new IllegalStateException("Element not found before waitMs elapsed");
    }

    private AccessibilityNodeInfo search(AccessibilityNodeInfo node, JSONObject selector, int[] index, Budget budget, int depth) throws Exception {
        if (!budget.enter(depth)) return null;
        if (matches(node, selector) && index[0]-- == 0) return AccessibilityNodeInfo.obtain(node);
        for (int i = 0; i < node.getChildCount(); i++) {
            // Check before getChild(): returning early in the recursive call still
            // fetches every remaining sibling through Android's remote interface.
            if (!budget.available(depth + 1)) break;
            AccessibilityNodeInfo child = node.getChild(i);
            if (child == null) continue;
            try {
                AccessibilityNodeInfo found = search(child, selector, index, budget, depth + 1);
                if (found != null) return found;
            } finally { child.recycle(); }
        }
        return null;
    }

    private static boolean matches(AccessibilityNodeInfo node, JSONObject s) throws Exception {
        return node.isVisibleToUser()
                && (!s.has("resourceId") || s.getString("resourceId").equals(string(node.getViewIdResourceName())))
                && (!s.has("text") || !node.isPassword() && s.getString("text").equals(string(node.getText())))
                && (!s.has("description") || s.getString("description").equals(string(node.getContentDescription())))
                && (!s.has("className") || s.getString("className").equals(string(node.getClassName())));
    }

    JSONObject setText(JSONObject request) throws Exception {
        AccessibilityNodeInfo node = find(request);
        try {
            if (!node.isEditable() || !node.isEnabled()) throw new IllegalStateException("Element is not an enabled editable field");
            Bundle args = new Bundle();
            args.putCharSequence(AccessibilityNodeInfo.ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE, request.getString("text"));
            if (!node.performAction(AccessibilityNodeInfo.ACTION_SET_TEXT, args))
                throw new IllegalStateException("Element rejected ACTION_SET_TEXT");
            node.refresh();
            return describe(node);
        } finally { node.recycle(); }
    }

    JSONObject tree(JSONObject request) throws Exception {
        String format = request.optString("format", "json");
        if (!"json".equals(format) && !"xml".equals(format)) throw new IllegalArgumentException("format must be json or xml");
        platform.automation();
        long started = SystemClock.uptimeMillis();
        Budget budget = new Budget(started + TRAVERSAL_MS);
        AccessibilityNodeInfo root = platform.requireActiveRoot(TRAVERSAL_MS);
        JSONObject result = new JSONObject();
        try {
            int rotation = platform.geometry().rotation;
            result.put("format", format).put("rotation", rotation);
            if ("xml".equals(format)) {
                StringWriter out = new StringWriter();
                XmlSerializer xml = Xml.newSerializer();
                xml.setOutput(out);
                xml.startDocument("UTF-8", true);
                xml.startTag("", "hierarchy");
                xml.attribute("", "rotation", String.valueOf(rotation));
                dump(root, 0, budget, xml);
                xml.endTag("", "hierarchy"); xml.endDocument();
                result.put("xml", out.toString());
            } else result.put("root", dump(root, 0, budget, null));
        } finally { root.recycle(); }
        result.put("nodeCount", MAX_NODES - budget.remaining).put("truncated", budget.reason != null);
        if (budget.reason != null) {
            result.put("truncationReason", budget.reason);
            System.err.println("Marionette ui-tree-truncated elapsedMs=" + (SystemClock.uptimeMillis() - started)
                    + " nodeCount=" + (MAX_NODES - budget.remaining) + " reason=" + budget.reason);
        }
        // Control replies have a 1 MiB frame limit. Never corrupt the control connection with an oversized dump.
        if (result.toString().getBytes("UTF-8").length > 900000)
            throw new IllegalStateException("UI tree exceeds control limit; use a selector instead");
        return result;
    }

    private JSONObject dump(AccessibilityNodeInfo node, int depth, Budget budget, XmlSerializer xml) throws Exception {
        // Always include an acquired root, even when its Binder read used the budget.
        budget.remaining--;
        JSONObject value = describe(node);
        if (xml != null) {
            xml.startTag("", "node");
            for (Iterator<String> it = value.keys(); it.hasNext();) {
                String key = it.next(); xml.attribute("", key, value.get(key).toString());
            }
        }
        JSONArray children = new JSONArray();
        for (int i = 0; i < node.getChildCount(); i++) {
            if (!budget.available(depth + 1)) break;
            AccessibilityNodeInfo child = node.getChild(i);
            if (child == null) continue;
            try { children.put(dump(child, depth + 1, budget, xml)); } finally { child.recycle(); }
        }
        if (xml != null) xml.endTag("", "node");
        else value.put("children", children);
        return value;
    }

    private static IllegalStateException incomplete(Budget budget, long started) {
        System.err.println("Marionette element-search-incomplete elapsedMs=" + (SystemClock.uptimeMillis() - started)
                + " nodeCount=" + (MAX_NODES - budget.remaining) + " reason=" + budget.reason);
        return new IllegalStateException("Element search incomplete: " + budget.reason + "; use a more specific selector");
    }

    private static final class Budget {
        final long deadline;
        int remaining = MAX_NODES;
        String reason;
        Budget(long deadline) { this.deadline = deadline; }
        boolean available(int depth) {
            if (reason != null) return false;
            if (remaining <= 0) reason = "nodeLimit";
            else if (depth > MAX_DEPTH) reason = "depthLimit";
            else if (SystemClock.uptimeMillis() >= deadline) reason = "timeLimit";
            return reason == null;
        }
        boolean enter(int depth) {
            if (!available(depth)) return false;
            remaining--;
            return true;
        }
    }

    static JSONObject describe(AccessibilityNodeInfo node) throws Exception {
        Rect b = new Rect(); node.getBoundsInScreen(b);
        return new JSONObject().put("resourceId", string(node.getViewIdResourceName()))
                .put("text", node.isPassword() ? "" : string(node.getText()))
                .put("description", node.isPassword() ? "" : string(node.getContentDescription()))
                .put("className", string(node.getClassName())).put("package", string(node.getPackageName()))
                .put("enabled", node.isEnabled()).put("clickable", node.isClickable())
                .put("editable", node.isEditable()).put("focused", node.isFocused()).put("password", node.isPassword())
                .put("bounds", new JSONObject().put("left", b.left).put("top", b.top).put("right", b.right).put("bottom", b.bottom));
    }

    private static String string(CharSequence value) { return value == null ? "" : value.toString(); }
    private static int number(JSONObject o, String key, int fallback, int min, int max) throws Exception {
        if (!o.has(key)) return fallback;
        Object value = o.get(key);
        if (!(value instanceof Number)) throw new IllegalArgumentException(key + " must be an integer");
        double n = ((Number) value).doubleValue();
        if (n < min || n > max || n != Math.rint(n)) throw new IllegalArgumentException(key + " outside " + min + ".." + max);
        return (int) n;
    }
}
