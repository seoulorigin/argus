// Package detect turns the hub's raw observations (node polls, Pod usage, the
// Kubernetes view) into service/node states and incidents.
//
// A rule only opens an incident after it was violated in Trigger of the last
// Window evaluations (so one bad poll never fires it) and only closes it after
// Recover consecutive healthy evaluations (so one good poll never clears it).
// Evaluations run every 5 seconds.
package detect

import "time"

type Severity string

const (
	Warning  Severity = "WARNING"
	Critical Severity = "CRITICAL"
)

// Status is the state shown for a service or node.
type Status string

const (
	Up      Status = "UP"
	Warn    Status = "WARN"
	Down    Status = "DOWN"
	Unknown Status = "UNKNOWN" // not enough information to judge
)

const (
	RuleServiceDown     = "service_down"
	RuleServiceDegraded = "service_degraded"
	RuleUnreachable     = "service_unreachable"
	RuleProbeErrors     = "service_errors"
	RuleLatency         = "service_latency_high"
	RuleLinkFailed      = "pod_link_failed"
	RulePodCPU          = "pod_cpu_high"
	RulePodMemory       = "pod_memory_high"
	RulePodUnhealthy    = "pod_unhealthy"
	RuleNodeDown        = "node_down"
	RuleNodeCPU         = "node_cpu_high"
	RuleNodeMemory      = "node_memory_high"
	RuleNodePressure    = "node_pressure"
)

const (
	TargetService = "service"
	TargetPod     = "pod"
	TargetNode    = "node"
)

// Config holds the thresholds. Percentages are 0-100.
type Config struct {
	PodCPUPct  float64 // Pod CPU use as a share of its CPU limit
	PodMemPct  float64 // Pod working set as a share of its memory limit
	NodeCPUPct float64
	NodeMemPct float64
	// Probe rules look at the last 30 seconds of service probes from every node.
	ProbeDownPct float64 // failure rate above this means the service is unreachable
	ProbeWarnPct float64 // failure rate above this (but not unreachable) is a warning
	ProbeP95Ms   float64 // 95th percentile response time above this is a warning
	// Hysteresis is the fraction of a threshold a value must fall below to count
	// as recovered: with 0.8 and a 90% threshold, 72% is healthy, 80% is neither.
	Hysteresis float64
}

func DefaultConfig() Config {
	return Config{PodCPUPct: 85, PodMemPct: 90, NodeCPUPct: 90, NodeMemPct: 90, ProbeDownPct: 50, ProbeWarnPct: 10, ProbeP95Ms: 250, Hysteresis: 0.8}
}

// Incident is one detected problem, from its start until it recovers.
type Incident struct {
	ID          string         `json:"id"`
	Rule        string         `json:"rule"`
	TargetType  string         `json:"targetType"`
	Target      string         `json:"target"`
	Severity    Severity       `json:"severity"`
	Open        bool           `json:"open"`
	Threshold   string         `json:"threshold"` // the condition that is violated, e.g. "ready endpoints = 0"
	Observed    string         `json:"observed"`  // the latest violating value
	Peak        float64        `json:"peak,omitempty"`
	Window      string         `json:"window"` // how the rule decided, e.g. "3 of last 5 checks (5s each)"
	StartedAt   time.Time      `json:"startedAt"`
	RecoveredAt *time.Time     `json:"recoveredAt,omitempty"`
	Note        string         `json:"note,omitempty"`
	Detail      map[string]any `json:"detail,omitempty"`
}

type EventKind string

const (
	Opened    EventKind = "opened"
	Updated   EventKind = "updated" // peak value rose while open
	Recovered EventKind = "recovered"
)

type Event struct {
	Kind     EventKind
	Incident Incident
}

type ServiceStatus struct {
	Key       string   `json:"key"`
	Namespace string   `json:"ns"`
	Name      string   `json:"name"`
	Status    Status   `json:"status"`
	Ready     int      `json:"ready"`
	Total     int      `json:"total"`
	Pods      int      `json:"pods"`
	Reason    string   `json:"reason,omitempty"`
	Incidents []string `json:"incidents,omitempty"`
	// Usage is the sum over the service's Pods. Limits are 0 unless every Pod sets one.
	CPUMilli      float64 `json:"cpuMilli"`
	MemBytes      uint64  `json:"memBytes"`
	CPULimitMilli int64   `json:"cpuLimitMilli,omitempty"`
	MemLimitBytes int64   `json:"memLimitBytes,omitempty"`
	// Probe results over the last 30 seconds, from all nodes. ProbeN is 0 while
	// there are too few samples to say anything.
	ProbeN     int     `json:"probeN"`
	P95Ms      float64 `json:"p95Ms"`
	FailPct    float64 `json:"failPct"`
	ProbeError string  `json:"probeError,omitempty"` // most frequent failure class
}

type NodeStatus struct {
	Name      string   `json:"name"`
	Status    Status   `json:"status"`
	CPUPct    float64  `json:"cpu"`
	MemPct    float64  `json:"mem"`
	Reason    string   `json:"reason,omitempty"`
	Incidents []string `json:"incidents,omitempty"`
}

// Point is one sample of a target's history. For a service C is millicores and M
// is bytes; for a node C and M are percentages.
type Point struct {
	T int64   `json:"t"` // unix milliseconds
	C float64 `json:"c"`
	M float64 `json:"m"`
	// Services only: probe p95 in milliseconds and failure percentage; absent
	// when there were too few samples.
	P *float64 `json:"p,omitempty"`
	F *float64 `json:"f,omitempty"`
}

// View is the engine's current picture, rebuilt on every evaluation.
type View struct {
	At       time.Time       `json:"at"`
	Services []ServiceStatus `json:"services"`
	Nodes    []NodeStatus    `json:"nodes"`
	Open     []Incident      `json:"open"`
	Recent   []Incident      `json:"recent"` // recovered, newest first
}
