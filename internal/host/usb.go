package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"howett.net/plist"
)

// USBDevices reports hardware presence separately from ADB authorization. A USB
// device is never eligible for a lease until ADB reports its state as "device".
func (a ExecADB) USBDevices(ctx context.Context) ([]Device, error) {
	switch runtime.GOOS {
	case "darwin":
		data, err := exec.CommandContext(ctx, "ioreg", "-r", "-c", "IOUSBHostDevice", "-a").Output()
		if err != nil {
			return nil, err
		}
		var root any
		if _, err := plist.Unmarshal(data, &root); err != nil {
			return nil, err
		}
		var devices []Device
		walkUSB(root, func(node map[string]any) {
			serial, _ := node["USB Serial Number"].(string)
			product, _ := node["USB Product Name"].(string)
			if validUSBSerial(serial) && (strings.Contains(strings.ToLower(product), "android") || hasADBInterface(node)) {
				devices = append(devices, Device{Serial: serial, Model: product, Platform: "android", State: "usb"})
			}
		})
		return devices, nil
	case "linux":
		return linuxUSBDevices()
	default:
		return nil, nil
	}
}

func validUSBSerial(serial string) bool {
	return serial != "" && len(serial) <= 200 && serial != "." && serial != ".." && !strings.ContainsAny(serial, "/\\\x00\r\n\t ")
}

func walkUSB(value any, visit func(map[string]any)) {
	switch node := value.(type) {
	case []any:
		for _, child := range node {
			walkUSB(child, visit)
		}
	case map[string]any:
		visit(node)
		walkUSB(node["IORegistryEntryChildren"], visit)
	}
}

func hasADBInterface(node map[string]any) bool {
	found := false
	walkUSB(node["IORegistryEntryChildren"], func(child map[string]any) {
		name, _ := child["IORegistryEntryName"].(string)
		if strings.Contains(strings.ToLower(name), "adb interface") {
			found = true
		}
	})
	return found
}

func linuxUSBDevices() ([]Device, error) {
	const root = "/sys/bus/usb/devices"
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	read := func(name, field string) string {
		data, _ := os.ReadFile(filepath.Join(root, name, field))
		return strings.TrimSpace(string(data))
	}
	adb := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if device, _, ok := strings.Cut(name, ":"); ok && read(name, "bInterfaceClass") == "ff" && read(name, "bInterfaceSubClass") == "42" && read(name, "bInterfaceProtocol") == "01" {
			adb[device] = true
		}
	}
	var devices []Device
	for _, entry := range entries {
		name := entry.Name()
		if strings.Contains(name, ":") {
			continue
		}
		serial, product := read(name, "serial"), read(name, "product")
		if validUSBSerial(serial) && (adb[name] || strings.Contains(strings.ToLower(product), "android")) {
			devices = append(devices, Device{Serial: serial, Model: product, Platform: "android", State: "usb"})
		}
	}
	return devices, nil
}
