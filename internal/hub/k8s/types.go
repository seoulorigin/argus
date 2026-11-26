// Package k8s gives the hub a read-only, periodically refreshed view of the
// cluster: nodes, pods, services and which pods currently back each service.
package k8s

import "time"

// Pod is the part of a Kubernetes Pod that monitoring needs.
type Pod struct {
	UID         string            `json:"uid"`
	Namespace   string            `json:"ns"`
	Name        string            `json:"name"`
	Node        string            `json:"node"`
	IP          string            `json:"ip,omitempty"`
	Phase       string            `json:"phase"`
	Ready       bool              `json:"ready"`
	Terminating bool              `json:"terminating,omitempty"`
	HostNetwork bool              `json:"hostNetwork,omitempty"`
	Restarts    int32             `json:"restarts"`
	Reason      string            `json:"reason,omitempty"` // e.g. CrashLoopBackOff, OOMKilled
	Owner       string            `json:"owner,omitempty"`  // e.g. Deployment/frontend (via ReplicaSet)
	Labels      map[string]string `json:"labels,omitempty"`

	CPURequestMilli int64 `json:"cpuReqMilli"`
	CPULimitMilli   int64 `json:"cpuLimMilli"` // 0 when any container has no limit
	MemRequestBytes int64 `json:"memReq"`
	MemLimitBytes   int64 `json:"memLim"` // 0 when any container has no limit
}

// FullName is "namespace/name", the label used for a Pod everywhere in the UI.
func (p Pod) FullName() string { return p.Namespace + "/" + p.Name }

// Service is a Kubernetes Service together with its current backends.
type Service struct {
	Namespace string            `json:"ns"`
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	ClusterIP string            `json:"clusterIP,omitempty"`
	Ports     []ServicePort     `json:"ports,omitempty"`
	Selector  map[string]string `json:"selector,omitempty"`

	// Probe and ProbePort are the values of the argus/probe and argus/probe-port annotations.
	Probe     string `json:"probe,omitempty"`
	ProbePort string `json:"probePort,omitempty"`

	// PodUIDs are the pods the selector matches (running or not).
	PodUIDs []string `json:"pods"`
	// ReadyEndpoints and TotalEndpoints come from the Service's EndpointSlices.
	// HasSlices is false when no EndpointSlice exists yet, in which case the
	// counts say nothing about health.
	ReadyEndpoints int  `json:"ready"`
	TotalEndpoints int  `json:"total"`
	HasSlices      bool `json:"hasSlices"`
	// ReadyPodUIDs are the pods that currently receive traffic.
	ReadyPodUIDs []string `json:"readyPods"`
}

type ServicePort struct {
	Name     string `json:"name,omitempty"`
	Port     int32  `json:"port"`
	Protocol string `json:"protocol,omitempty"`
}

// Key is "namespace/name".
func (s Service) Key() string { return s.Namespace + "/" + s.Name }

// Node is a cluster node.
type Node struct {
	Name           string `json:"name"`
	Ready          bool   `json:"ready"`
	Unschedulable  bool   `json:"unschedulable,omitempty"`
	IP             string `json:"ip,omitempty"`
	ControlPlane   bool   `json:"controlPlane,omitempty"`
	CPUMilli       int64  `json:"cpuMilli"`
	MemBytes       int64  `json:"mem"`
	ConditionsDown string `json:"conditionsDown,omitempty"` // e.g. "MemoryPressure,DiskPressure"
}

// Snapshot is one consistent view of the cluster.
type Snapshot struct {
	Taken    time.Time `json:"taken"`
	Nodes    []Node    `json:"nodes"`
	Pods     []Pod     `json:"pods"`
	Services []Service `json:"services"`
}

// PodByUID returns a lookup table for the snapshot's pods.
func (s *Snapshot) PodByUID() map[string]*Pod {
	m := make(map[string]*Pod, len(s.Pods))
	for i := range s.Pods {
		m[s.Pods[i].UID] = &s.Pods[i]
	}
	return m
}
