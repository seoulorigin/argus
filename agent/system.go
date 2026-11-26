package agent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"argus"
	"argus/agent/utils"
	"argus/internal/entities/system"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
)

// Sets initial / non-changing values about the host system
func (a *Agent) refreshSystemDetails() {
	a.systemInfo.AgentVersion = argus.Version

	var hostInfo struct {
		OperatingSystem, KernelVersion string
		NCPU                           int
		MemTotal                       uint64
	}

	a.systemDetails.Hostname, _ = os.Hostname()
	if arch, err := host.KernelArch(); err == nil {
		a.systemDetails.Arch = arch
	} else {
		a.systemDetails.Arch = runtime.GOARCH
	}

	platform, _, version, _ := host.PlatformInformation()

	if platform == "darwin" {
		a.systemDetails.Os = system.Darwin
		a.systemDetails.OsName = fmt.Sprintf("macOS %s", version)
	} else if strings.Contains(platform, "indows") {
		a.systemDetails.Os = system.Windows
		a.systemDetails.OsName = strings.Replace(platform, "Microsoft ", "", 1)
		a.systemDetails.Kernel = version
	} else if platform == "freebsd" {
		a.systemDetails.Os = system.Freebsd
		a.systemDetails.Kernel, _ = host.KernelVersion()
		if prettyName, err := getOsPrettyName(); err == nil {
			a.systemDetails.OsName = prettyName
		} else {
			a.systemDetails.OsName = "FreeBSD"
		}
	} else {
		a.systemDetails.Os = system.Linux
		a.systemDetails.OsName = hostInfo.OperatingSystem
		if a.systemDetails.OsName == "" {
			if prettyName, err := getOsPrettyName(); err == nil {
				a.systemDetails.OsName = prettyName
			} else {
				a.systemDetails.OsName = platform
			}
		}
		a.systemDetails.Kernel = hostInfo.KernelVersion
		if a.systemDetails.Kernel == "" {
			a.systemDetails.Kernel, _ = host.KernelVersion()
		}
	}

	// cpu model
	if info, err := cpu.Info(); err == nil && len(info) > 0 {
		a.systemDetails.CpuModel = info[0].ModelName
		a.systemDetails.Sockets = cpuSockets(info)
	}
	// gopsutil doesn't parse the "cpu model" field from /proc/cpuinfo, which
	// is the only source of the CPU model name on MIPS. Fall back to reading
	// it directly when ModelName is empty.
	if a.systemDetails.CpuModel == "" {
		a.systemDetails.CpuModel = getCpuModelFromCpuinfo()
	}
	// cores / threads
	cores, _ := cpu.Counts(false)
	threads := hostInfo.NCPU
	if threads == 0 {
		threads, _ = cpu.Counts(true)
	}
	// Container cpusets describe assigned logical CPUs; Docker may report a stale count.
	if count := containerCpuCount(a.forceUseCgroup); count > 0 {
		threads = count
	}
	// in lxc, logical cores reflects container limits, so use that as cores if lower
	if threads > 0 && threads < cores {
		cores = threads
	}
	a.systemDetails.Cores = cores
	a.systemDetails.Threads = threads

	// total memory
	a.systemDetails.MemoryTotal = hostInfo.MemTotal
	hostMemoryTotal := hostInfo.MemTotal
	if v, err := memoryVirtualMemory(); err == nil {
		hostMemoryTotal = v.Total
		if a.systemDetails.MemoryTotal == 0 {
			a.systemDetails.MemoryTotal = v.Total
		}
	}
	if metrics, ok := containerMemoryMetrics(hostMemoryTotal, a.forceUseCgroup); ok {
		a.systemDetails.MemoryTotal = metrics.Total
	}

}

// attachSystemDetails returns details only for fresh default-interval responses.
func (a *Agent) attachSystemDetails(data *system.CombinedData, cacheTimeMs uint16, includeRequested bool) *system.CombinedData {
	if cacheTimeMs != defaultDataCacheTimeMs || (!includeRequested && !a.detailsDirty) {
		return data
	}

	// copy data to avoid adding details to the original cached struct
	response := *data
	response.Details = &a.systemDetails
	a.detailsDirty = false
	return &response
}

// updateSystemDetails applies a mutation to the static details payload and marks
// it for inclusion on the next fresh default-interval response.
func (a *Agent) updateSystemDetails(updateFunc func(details *system.Details)) {
	updateFunc(&a.systemDetails)
	a.detailsDirty = true
}

// Returns current info, stats about the host system
func (a *Agent) getSystemStats(cacheTimeMs uint16) system.Stats {
	var systemStats system.Stats

	// cpu metrics
	cpuMetrics, err := getCpuMetrics(cacheTimeMs, a.forceUseCgroup)
	if err == nil {
		systemStats.Cpu = utils.TwoDecimals(cpuMetrics.Total)
		systemStats.CpuBreakdown = []float64{
			utils.TwoDecimals(cpuMetrics.User),
			utils.TwoDecimals(cpuMetrics.System),
			utils.TwoDecimals(cpuMetrics.Iowait),
			utils.TwoDecimals(cpuMetrics.Steal),
			utils.TwoDecimals(cpuMetrics.Idle),
			utils.TwoDecimals(cpuMetrics.Irq),
			utils.TwoDecimals(cpuMetrics.Softirq),
			utils.TwoDecimals(cpuMetrics.Nice),
		}
	} else {
		slog.Error("Error getting cpu metrics", "err", err)
	}

	// per-core cpu usage. Skipped when the total comes from cgroup accounting:
	// per-core /proc/stat counters there describe shared host cores, not the
	// guest, and would contradict the total.
	if !cpuMetrics.fromCgroup {
		if perCoreUsage, err := getPerCoreCpuUsage(cacheTimeMs); err == nil {
			systemStats.CpuCoresUsage = perCoreUsage
		}
	}

	// load average
	if avgstat, err := load.Avg(); err == nil {
		systemStats.LoadAvg[0] = utils.TwoDecimals(avgstat.Load1)
		systemStats.LoadAvg[1] = utils.TwoDecimals(avgstat.Load5)
		systemStats.LoadAvg[2] = utils.TwoDecimals(avgstat.Load15)
		slog.Debug("Load average", "5m", avgstat.Load5, "15m", avgstat.Load15)
	} else {
		slog.Error("Error getting load average", "err", err)
	}

	// memory
	a.updateMemoryStats(&systemStats)

	// network stats (per cache interval)
	a.updateNetworkStats(cacheTimeMs, &systemStats)

	// update system info
	a.systemInfo.Cpu = systemStats.Cpu
	a.systemInfo.LoadAvg = systemStats.LoadAvg
	a.systemInfo.MemPct = systemStats.MemPct
	a.systemInfo.Uptime, _ = getUptime()
	a.systemInfo.BandwidthBytes = systemStats.Bandwidth[0] + systemStats.Bandwidth[1]
	a.systemInfo.Threads = a.systemDetails.Threads

	return systemStats
}

// cpuModelFallbackKeys are the field names to look for in /proc/cpuinfo when
// gopsutil fails to return a ModelName. The "cpu model" key is used on MIPS
// (e.g. "MIPS 1004Kc V2.15"), while "system type" provides SoC information
// on various embedded architectures.
var cpuModelFallbackKeys = []string{"cpu model", "system type"}

// cpuSockets returns the number of physical CPU packages, or 0 if unknown.
// PhysicalID is only a socket id on x86 Linux; on Windows gopsutil returns
// one entry per package.
func cpuSockets(info []cpu.InfoStat) int {
	switch {
	case runtime.GOOS == "windows":
		return len(info)
	case runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "386"):
		ids := make(map[string]struct{})
		for _, c := range info {
			if c.PhysicalID != "" {
				ids[c.PhysicalID] = struct{}{}
			}
		}
		return len(ids)
	}
	return 0
}

// getCpuModelFromCpuinfo reads /proc/cpuinfo and returns a CPU model string.
// This is a fallback for architectures where gopsutil's cpu.Info() does not
// populate ModelName, most notably MIPS.
func getCpuModelFromCpuinfo() string {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer file.Close()
	return parseCpuModel(file)
}

// parseCpuModel scans r (expected to be /proc/cpuinfo content) and returns
// a combined CPU model string. It collects values from all matching keys
// and joins them with " / " when multiple are found.
func parseCpuModel(r io.Reader) string {
	lines := readLines(r)
	var parts []string
	for _, key := range cpuModelFallbackKeys {
		for _, line := range lines {
			after, found := strings.CutPrefix(line, key)
			if !found {
				continue
			}
			after = strings.TrimSpace(after)
			if len(after) < 2 || after[0] != ':' {
				continue
			}
			if value := strings.TrimSpace(after[1:]); value != "" {
				parts = append(parts, value)
				break
			}
		}
	}
	return strings.Join(parts, " / ")
}

// readLines reads all lines from r into a slice.
func readLines(r io.Reader) []string {
	scanner := bufio.NewScanner(r)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

// calculateHostMemoryUsage derives counters defensively because /proc/meminfo may
// change while gopsutil reads it. Invalid unsigned subtractions saturate at zero.
func calculateHostMemoryUsage(v *mem.VirtualMemoryStat, htop bool) (used, cacheBuff, swapUsed uint64) {
	used = v.Used
	if used > v.Total {
		used = saturatingSub(v.Total, v.Available)
	}

	// gopsutil automatically adds SReclaimable to Cached.
	cacheBuff = min(v.Cached, v.Total)
	cacheBuff += min(v.Buffers, v.Total-cacheBuff)
	cacheBuff = saturatingSub(cacheBuff, min(v.Shared, v.Total))
	if v.Cached == 0 && v.Buffers == 0 {
		cacheBuff = saturatingSub(v.Total, v.Free, used)
	}
	if htop {
		used = saturatingSub(v.Total, v.Free, cacheBuff)
	}
	// Cached swap pages still occupy swap slots and are included in `free`'s used value.
	return used, cacheBuff, saturatingSub(v.SwapTotal, v.SwapFree)
}

// saturatingSub subtracts each value, returning zero on underflow.
func saturatingSub(value uint64, subtrahends ...uint64) uint64 {
	for _, subtrahend := range subtrahends {
		if subtrahend > value {
			return 0
		}
		value -= subtrahend
	}
	return value
}

// getOsPrettyName attempts to get the pretty OS name from /etc/os-release on Linux systems
func getOsPrettyName() (string, error) {
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			value := after
			value = strings.Trim(value, `"`)
			return value, nil
		}
	}

	return "", errors.New("pretty name not found")
}
