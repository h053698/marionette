package machine

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Metrics is a point-in-time host resource reading. Zero values mean the
// number is unavailable on this OS, so the UI must check the matching Has flag.
type Metrics struct {
	SampledAt   time.Time `json:"sampledAt"`
	CPUPercent  float64   `json:"cpuPercent"`
	HasCPU      bool      `json:"hasCpu"`
	Load1       float64   `json:"load1"`
	Load5       float64   `json:"load5"`
	Load15      float64   `json:"load15"`
	HasLoad     bool      `json:"hasLoad"`
	MemoryTotal uint64    `json:"memoryTotal"`
	MemoryUsed  uint64    `json:"memoryUsed"`
	HasMemory   bool      `json:"hasMemory"`
	DiskTotal   uint64    `json:"diskTotal"`
	DiskUsed    uint64    `json:"diskUsed"`
	DiskPath    string    `json:"diskPath,omitempty"`
	HasDisk     bool      `json:"hasDisk"`
	UptimeSec   int64     `json:"uptimeSec"`
	HasUptime   bool      `json:"hasUptime"`
}

const metricsTTL = 5 * time.Second

var metricsCache struct {
	sync.Mutex
	at   time.Time
	path string
	data Metrics
	cpu  cpuTicks
}

type cpuTicks struct{ busy, total uint64 }

// Metrics returns a cached reading no older than five seconds. Shell tools are
// bounded by a short timeout, so a slow OS call cannot stall the agent API.
func (m *Manager) Metrics() Metrics {
	return readMetrics(m.cfg.StateDir)
}

func readMetrics(stateDir string) Metrics {
	metricsCache.Lock()
	defer metricsCache.Unlock()
	if metricsCache.path == stateDir && time.Since(metricsCache.at) < metricsTTL {
		return metricsCache.data
	}
	out := Metrics{SampledAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "linux":
		readLinux(&out)
	case "darwin":
		readDarwin(ctx, &out)
	}
	var fs syscall.Statfs_t
	if stateDir != "" && syscall.Statfs(stateDir, &fs) == nil && fs.Blocks > 0 {
		size := uint64(fs.Bsize)
		out.DiskTotal = fs.Blocks * size
		out.DiskUsed = (fs.Blocks - fs.Bavail) * size
		out.DiskPath, out.HasDisk = stateDir, true
	}
	if out.HasCPU && runtime.GOOS == "linux" {
		// CPU percent is a delta between two samples, so the first call after
		// start reports unavailable instead of a misleading since-boot average.
		now, ok := linuxTicks()
		if ok && metricsCache.cpu.total > 0 && now.total > metricsCache.cpu.total {
			out.CPUPercent = 100 * float64(now.busy-metricsCache.cpu.busy) / float64(now.total-metricsCache.cpu.total)
		} else {
			out.HasCPU = false
		}
		if ok {
			metricsCache.cpu = now
		}
	}
	metricsCache.at, metricsCache.path, metricsCache.data = time.Now(), stateDir, out
	return out
}

func linuxTicks() (cpuTicks, bool) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTicks{}, false
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTicks{}, false
	}
	var total, idle uint64
	for i, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuTicks{}, false
		}
		if i < 8 { // user..steal; guest ticks are already inside user.
			total += value
		}
		if i == 3 || i == 4 { // idle + iowait
			idle += value
		}
	}
	return cpuTicks{busy: total - idle, total: total}, true
}

func readLinux(out *Metrics) {
	out.HasCPU = true
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) >= 3 {
			a, e1 := strconv.ParseFloat(fields[0], 64)
			b, e2 := strconv.ParseFloat(fields[1], 64)
			c, e3 := strconv.ParseFloat(fields[2], 64)
			if e1 == nil && e2 == nil && e3 == nil {
				out.Load1, out.Load5, out.Load15, out.HasLoad = a, b, c, true
			}
		}
	}
	if file, err := os.Open("/proc/meminfo"); err == nil {
		defer file.Close()
		var total, available uint64
		var haveTotal, haveAvailable bool
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			name, rest, _ := strings.Cut(scanner.Text(), ":")
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				continue
			}
			value, err := strconv.ParseUint(fields[0], 10, 64)
			if err != nil {
				continue
			}
			switch name {
			case "MemTotal":
				total, haveTotal = value*1024, true
			case "MemAvailable":
				available, haveAvailable = value*1024, true
			}
		}
		if haveTotal && haveAvailable && total >= available {
			out.MemoryTotal, out.MemoryUsed, out.HasMemory = total, total-available, true
		}
	}
	if raw, err := os.ReadFile("/proc/uptime"); err == nil {
		if seconds, err := strconv.ParseFloat(strings.Fields(string(raw))[0], 64); err == nil {
			out.UptimeSec, out.HasUptime = int64(seconds), true
		}
	}
}

func run(ctx context.Context, name string, args ...string) string {
	raw, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(raw)
}

func readDarwin(ctx context.Context, out *Metrics) {
	if raw := strings.Trim(strings.TrimSpace(run(ctx, "/usr/sbin/sysctl", "-n", "vm.loadavg")), "{} "); raw != "" {
		fields := strings.Fields(raw)
		if len(fields) >= 3 {
			a, e1 := strconv.ParseFloat(fields[0], 64)
			b, e2 := strconv.ParseFloat(fields[1], 64)
			c, e3 := strconv.ParseFloat(fields[2], 64)
			if e1 == nil && e2 == nil && e3 == nil {
				out.Load1, out.Load5, out.Load15, out.HasLoad = a, b, c, true
			}
		}
	}
	// Summing per-process %cpu is an instantaneous estimate that needs no
	// second sample, divided by core count to read as a whole-machine percent.
	if raw := run(ctx, "/bin/ps", "-A", "-o", "%cpu="); raw != "" {
		var sum float64
		for _, field := range strings.Fields(raw) {
			if value, err := strconv.ParseFloat(field, 64); err == nil {
				sum += value
			}
		}
		percent := sum / float64(runtime.NumCPU())
		if percent > 100 {
			percent = 100
		}
		out.CPUPercent, out.HasCPU = percent, true
	}
	total, err := strconv.ParseUint(strings.TrimSpace(run(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize")), 10, 64)
	if err == nil && total > 0 {
		page := uint64(syscall.Getpagesize())
		if size, err := strconv.ParseUint(strings.TrimSpace(run(ctx, "/usr/sbin/sysctl", "-n", "hw.pagesize")), 10, 64); err == nil && size > 0 {
			page = size
		}
		var free, speculative, purgeable, fileBacked uint64
		for _, line := range strings.Split(run(ctx, "/usr/bin/vm_stat"), "\n") {
			name, rest, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), "."), 10, 64)
			if err != nil {
				continue
			}
			switch name {
			case "Pages free":
				free = value
			case "Pages speculative":
				speculative = value
			case "Pages purgeable":
				purgeable = value
			case "File-backed pages":
				fileBacked = value
			}
		}
		// "Available" follows Activity Monitor: free, speculative, purgeable
		// and file cache can all be reclaimed without swapping.
		available := (free + speculative + purgeable + fileBacked) * page
		if available > total {
			available = total
		}
		out.MemoryTotal, out.MemoryUsed, out.HasMemory = total, total-available, true
	}
	if raw := run(ctx, "/usr/sbin/sysctl", "-n", "kern.boottime"); raw != "" {
		if _, rest, ok := strings.Cut(raw, "sec = "); ok {
			if value, _, _ := strings.Cut(rest, ","); value != "" {
				if boot, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && boot > 0 {
					out.UptimeSec, out.HasUptime = time.Now().Unix()-boot, true
				}
			}
		}
	}
}
