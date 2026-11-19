package dev.marionette;

import android.content.Intent;
import android.content.pm.ApplicationInfo;
import android.content.pm.PackageManager;
import android.content.pm.ResolveInfo;
import android.graphics.Bitmap;
import android.graphics.Canvas;
import android.graphics.drawable.Drawable;
import android.util.Base64;
import java.io.ByteArrayOutputStream;
import java.text.Collator;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Comparator;
import java.util.HashSet;
import java.util.List;
import org.json.JSONArray;
import org.json.JSONObject;

/** Launcher applications for the shell context's Android user. */
final class Apps {
    private final Platform platform;
    Apps(Platform platform) { this.platform = platform; }

    JSONObject list() throws Exception {
        PackageManager manager = platform.context.getPackageManager();
        Intent query = new Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER);
        List<JSONObject> apps = new ArrayList<>();
        HashSet<String> seen = new HashSet<>();
        boolean truncated = false;
        for (ResolveInfo info : manager.queryIntentActivities(query, 0)) {
            if (info.activityInfo == null || !info.activityInfo.enabled || !info.activityInfo.exported
                    || !info.activityInfo.applicationInfo.enabled) continue;
            String name = info.activityInfo.packageName;
            if (!seen.add(name)) continue;
            if (apps.size() >= 1000) { truncated = true; break; }
            CharSequence loaded = info.loadLabel(manager);
            String label = loaded == null ? name : loaded.toString();
            if (label.length() > 200) label = label.substring(0, 200);
            apps.add(new JSONObject().put("packageName", name).put("label", label)
                    .put("uid", info.activityInfo.applicationInfo.uid)
                    .put("system", (info.activityInfo.applicationInfo.flags & ApplicationInfo.FLAG_SYSTEM) != 0));
        }
        final Collator collator = Collator.getInstance();
        Collections.sort(apps, new Comparator<JSONObject>() {
            public int compare(JSONObject a, JSONObject b) {
                int order = collator.compare(a.optString("label"), b.optString("label"));
                return order != 0 ? order : a.optString("packageName").compareTo(b.optString("packageName"));
            }
        });
        return new JSONObject().put("apps", new JSONArray(apps)).put("truncated", truncated);
    }

    private void validatePackage(String name) {
        if (name.length() > 255 || !name.matches("[A-Za-z0-9_]+(?:\\.[A-Za-z0-9_]+)*"))
            throw new IllegalArgumentException("Invalid Android package name");
    }

    JSONObject icon(String name) throws Exception {
        validatePackage(name);
        Drawable drawable = platform.context.getPackageManager().getApplicationIcon(name);
        Bitmap bitmap = Bitmap.createBitmap(64, 64, Bitmap.Config.ARGB_8888);
        try {
            drawable.setBounds(0, 0, 64, 64);
            drawable.draw(new Canvas(bitmap));
            ByteArrayOutputStream bytes = new ByteArrayOutputStream();
            if (!bitmap.compress(Bitmap.CompressFormat.PNG, 100, bytes))
                throw new IllegalStateException("Could not render app icon");
            return new JSONObject().put("packageName", name).put("mimeType", "image/png")
                    .put("data", Base64.encodeToString(bytes.toByteArray(), Base64.NO_WRAP));
        } finally { bitmap.recycle(); }
    }

    JSONObject launch(String name) throws Exception {
        validatePackage(name);
        Intent intent = platform.context.getPackageManager().getLaunchIntentForPackage(name);
        if (intent == null) throw new IllegalArgumentException("App is not installed or has no launcher activity");
        intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK | Intent.FLAG_ACTIVITY_RESET_TASK_IF_NEEDED);
        platform.launch(intent);
        return new JSONObject().put("packageName", name).put("launched", true);
    }
}
