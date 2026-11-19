package dev.marionette;

import android.content.pm.ApplicationInfo;
import android.content.pm.PackageManager;
import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.util.HashMap;
import java.util.Map;
import org.json.JSONArray;
import org.json.JSONObject;

/** Read-only snapshot of the processes visible to the device's shell UID. */
final class Processes {
    private final Platform platform;
    Processes(Platform platform) { this.platform = platform; }

    JSONObject list() throws Exception {
        final java.lang.Process process = new ProcessBuilder("/system/bin/ps", "-A", "-o", "PID,PPID,UID,STAT,RSS,NAME")
                .redirectErrorStream(true).start();
        Thread watchdog = new Thread(new Runnable() {
            public void run() {
                try { Thread.sleep(4000); process.destroy(); }
                catch (InterruptedException ignored) { Thread.currentThread().interrupt(); }
            }
        }, "marionette-process-snapshot");
        watchdog.setDaemon(true);
        watchdog.start();
        JSONArray entries = new JSONArray();
        boolean truncated = false;
        Map<String, JSONObject> applications = new HashMap<>();
        PackageManager manager = platform.context.getPackageManager();
        try (BufferedReader reader = new BufferedReader(new InputStreamReader(process.getInputStream(), "UTF-8"))) {
            String header = reader.readLine();
            if (header == null || !header.trim().startsWith("PID"))
                throw new IllegalStateException("Device process listing is unavailable");
            int size = 0;
            int payloadBytes = 0;
            String line;
            while ((line = reader.readLine()) != null) {
                if (entries.length() >= 4000 || (size += line.length()) > 300000) { truncated = true; break; }
                String[] fields = line.trim().split("\\s+", 6);
                if (fields.length != 6) continue;
                JSONObject entry;
                try {
                    entry = new JSONObject().put("pid", Integer.parseInt(fields[0]))
                            .put("ppid", Integer.parseInt(fields[1])).put("uid", Integer.parseInt(fields[2]))
                            .put("state", fields[3]).put("rssKb", Long.parseLong(fields[4]))
                            .put("name", fields[5].substring(0, Math.min(512, fields[5].length())));
                } catch (NumberFormatException ignored) { continue; }
                String name = fields[5].split(":", 2)[0];
                if (name.indexOf('.') > 0 && name.matches("[A-Za-z0-9_]+(?:\\.[A-Za-z0-9_]+)*")) {
                    if (!applications.containsKey(name)) {
                        JSONObject application = new JSONObject();
                        try {
                            ApplicationInfo info = manager.getApplicationInfo(name, 0);
                            CharSequence label = manager.getApplicationLabel(info);
                            String text = label == null ? name : label.toString();
                            application.put("packageName", name).put("label", text.substring(0, Math.min(200, text.length())));
                        } catch (PackageManager.NameNotFoundException ignored) { }
                        applications.put(name, application);
                    }
                    JSONObject application = applications.get(name);
                    if (application.has("packageName")) {
                        entry.put("packageName", name).put("label", application.getString("label"));
                    }
                }
                payloadBytes += entry.toString().getBytes("UTF-8").length + 1;
                if (payloadBytes > 850000) { truncated = true; break; }
                entries.put(entry);
            }
            if (!truncated && process.waitFor() != 0)
                throw new IllegalStateException("Could not read device processes");
        } finally {
            watchdog.interrupt();
            process.destroy();
        }
        return new JSONObject().put("processes", entries).put("truncated", truncated)
                .put("capturedAt", System.currentTimeMillis());
    }
}
