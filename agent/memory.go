package agent

import (
	"runtime"

	"argus/agent/utils"
	"argus/internal/entities/system"
	"github.com/shirou/gopsutil/v4/mem"
)

// memoryMetrics holds byte counters. For cgroups, Used excludes file cache,
// while retaining shared memory/tmpfs. BuffCache is the subtracted file cache.
type memoryMetrics struct {
	Total, Used, BuffCache uint64
}

var memoryVirtualMemory = mem.VirtualMemory
var memorySwapMemory = mem.SwapMemory
var memoryGOOS = runtime.GOOS

// hostSwap returns total and free swap bytes. gopsutil's VirtualMemory only
// fills swap fields on Linux, so on every other OS they come from SwapMemory.
func hostSwap(v *mem.VirtualMemoryStat) (total, free uint64) {
	if v.SwapTotal == 0 && memoryGOOS != "linux" {
		if s, err := memorySwapMemory(); err == nil {
			return s.Total, s.Free
		}
	}
	return v.SwapTotal, v.SwapFree
}

func (a *Agent) updateMemoryStats(stats *system.Stats) {
	v, err := memoryVirtualMemory()
	var hostTotal uint64
	if err == nil {
		hostTotal = v.Total
		// Swap retains its existing /proc/meminfo source in both memory paths (SwapMemory on non-Linux).
		swapTotal, swapFree := hostSwap(v)
		stats.SwapBytes = [2]uint64{swapTotal, saturatingSub(swapTotal, swapFree)}
	}

	var arcSize uint64
	metrics, fromCgroup := containerMemoryMetrics(hostTotal, a.forceUseCgroup)
	if !fromCgroup {
		if err != nil {
			return
		}
		used, cacheBuff, _ := calculateHostMemoryUsage(v, a.memCalc == "htop")
		metrics = memoryMetrics{Total: v.Total, Used: used, BuffCache: cacheBuff}
	}

	stats.MemBytes = [4]uint64{metrics.Total, metrics.Used, metrics.BuffCache, arcSize}
	if metrics.Total > 0 {
		stats.MemPct = utils.TwoDecimals(float64(metrics.Used) / float64(metrics.Total) * 100)
	}
	if a.systemDetails.MemoryTotal != metrics.Total {
		a.updateSystemDetails(func(details *system.Details) {
			details.MemoryTotal = metrics.Total
		})
	}
}
