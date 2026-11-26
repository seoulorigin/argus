package systems

import (
	"time"

	"argus/internal/entities/probe"

	"argus/internal/entities/container"
	"argus/internal/entities/system"
)

// PodUsage is one Pod's resource usage as last reported by its node's agent.
type PodUsage struct {
	UID      string  // Kubernetes Pod UID
	Name     string  // "namespace/name" once the hub could resolve it, else the UID
	CPUPct   float64 // percent of the node's total CPU capacity
	MemBytes uint64  // working set
	NetSent  uint64  // bytes/s
	NetRecv  uint64  // bytes/s
}

// Observation is what the last successful poll of a node reported.
type Observation struct {
	At     time.Time
	CPUPct float64
	MemPct float64
	Pods   []PodUsage
}

// NodeObservation is a node as the detection engine sees it.
type NodeObservation struct {
	Name   string
	Status string // up | down | pending | paused
	Last   *Observation
	// Probes are the service probe samples this node reported since the previous call.
	Probes []probe.Sample
}

func (sys *System) storeObservation(data *system.CombinedData) {
	obs := &Observation{At: time.Now(), CPUPct: data.Info.Cpu, MemPct: data.Info.MemPct}
	obs.Pods = make([]PodUsage, 0, len(data.Containers))
	for _, c := range data.Containers {
		obs.Pods = append(obs.Pods, podUsageFrom(c))
	}
	sys.observed.Store(obs)
}

func podUsageFrom(c *container.Stats) PodUsage {
	return PodUsage{
		UID: c.Id, Name: c.Name, CPUPct: c.Cpu,
		MemBytes: uint64(c.Mem * 1024 * 1024),
		NetSent:  c.Bandwidth[0], NetRecv: c.Bandwidth[1],
	}
}

// Observations returns every monitored node with its latest poll.
func (sm *SystemManager) Observations() []NodeObservation {
	all := sm.systems.GetAll()
	out := make([]NodeObservation, 0, len(all))
	for _, sys := range all {
		out = append(out, NodeObservation{Name: sys.Name, Status: sys.GetStatus(), Last: sys.observed.Load(), Probes: sys.takeProbeSamples()})
	}
	return out
}

// maxPendingProbeSamples bounds memory if the detection engine stops reading.
const maxPendingProbeSamples = 5000

func (sys *System) addProbeSamples(samples []probe.Sample) {
	if len(samples) == 0 {
		return
	}
	sys.probeMu.Lock()
	defer sys.probeMu.Unlock()
	sys.probeSamples = append(sys.probeSamples, samples...)
	if over := len(sys.probeSamples) - maxPendingProbeSamples; over > 0 {
		sys.probeSamples = sys.probeSamples[over:]
	}
}

func (sys *System) takeProbeSamples() []probe.Sample {
	sys.probeMu.Lock()
	defer sys.probeMu.Unlock()
	out := sys.probeSamples
	sys.probeSamples = nil
	return out
}
