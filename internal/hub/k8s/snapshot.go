package k8s

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"
)

type objMeta struct {
	UID               string            `json:"uid"`
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	OwnerReferences   []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"ownerReferences"`
}

type rawPod struct {
	Metadata objMeta `json:"metadata"`
	Spec     struct {
		NodeName    string `json:"nodeName"`
		HostNetwork bool   `json:"hostNetwork"`
		Containers  []struct {
			Resources struct {
				Requests map[string]string `json:"requests"`
				Limits   map[string]string `json:"limits"`
			} `json:"resources"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		PodIP      string `json:"podIP"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			RestartCount int32 `json:"restartCount"`
			State        struct {
				Waiting    *struct{ Reason string } `json:"waiting"`
				Terminated *struct{ Reason string } `json:"terminated"`
			} `json:"state"`
			LastState struct {
				Terminated *struct{ Reason string } `json:"terminated"`
			} `json:"lastState"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type rawService struct {
	Metadata objMeta `json:"metadata"`
	Spec     struct {
		Type      string            `json:"type"`
		ClusterIP string            `json:"clusterIP"`
		Selector  map[string]string `json:"selector"`
		Ports     []ServicePort     `json:"ports"`
	} `json:"spec"`
}

type rawSlice struct {
	Metadata  objMeta `json:"metadata"`
	Endpoints []struct {
		Conditions struct {
			Ready *bool `json:"ready"`
		} `json:"conditions"`
		TargetRef *struct {
			Kind string `json:"kind"`
			UID  string `json:"uid"`
		} `json:"targetRef"`
	} `json:"endpoints"`
}

type rawNode struct {
	Metadata objMeta `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		Capacity   map[string]string `json:"capacity"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		Addresses []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
	} `json:"status"`
}

// Fetch lists the cluster and builds a consistent snapshot. Any failed list
// fails the whole fetch, so a snapshot is never half old and half new.
func (c *Client) Fetch(ctx context.Context) (*Snapshot, error) {
	var rawPods []rawPod
	var rawSvcs []rawService
	var rawSlices []rawSlice
	var rawNodes []rawNode
	collect := func(path string, add func(json.RawMessage) error) error { return c.list(ctx, path, add) }

	if err := collect("/api/v1/nodes", func(r json.RawMessage) error {
		var n rawNode
		err := json.Unmarshal(r, &n)
		rawNodes = append(rawNodes, n)
		return err
	}); err != nil {
		return nil, err
	}
	if err := collect("/api/v1/pods", func(r json.RawMessage) error {
		var p rawPod
		err := json.Unmarshal(r, &p)
		rawPods = append(rawPods, p)
		return err
	}); err != nil {
		return nil, err
	}
	if err := collect("/api/v1/services", func(r json.RawMessage) error {
		var s rawService
		err := json.Unmarshal(r, &s)
		rawSvcs = append(rawSvcs, s)
		return err
	}); err != nil {
		return nil, err
	}
	if err := collect("/apis/discovery.k8s.io/v1/endpointslices", func(r json.RawMessage) error {
		var s rawSlice
		err := json.Unmarshal(r, &s)
		rawSlices = append(rawSlices, s)
		return err
	}); err != nil {
		return nil, err
	}
	return buildSnapshot(rawNodes, rawPods, rawSvcs, rawSlices, time.Now()), nil
}

func buildSnapshot(nodes []rawNode, pods []rawPod, svcs []rawService, slices_ []rawSlice, now time.Time) *Snapshot {
	snap := &Snapshot{Taken: now}

	for _, n := range nodes {
		node := Node{
			Name:          n.Metadata.Name,
			Unschedulable: n.Spec.Unschedulable,
			CPUMilli:      milliCPU(n.Status.Capacity["cpu"]),
			MemBytes:      bytesOf(n.Status.Capacity["memory"]),
		}
		_, node.ControlPlane = n.Metadata.Labels["node-role.kubernetes.io/control-plane"]
		var bad []string
		for _, c := range n.Status.Conditions {
			switch {
			case c.Type == "Ready":
				node.Ready = c.Status == "True"
			case c.Status == "True":
				bad = append(bad, c.Type) // MemoryPressure, DiskPressure, PIDPressure, ...
			}
		}
		sort.Strings(bad)
		node.ConditionsDown = strings.Join(bad, ",")
		for _, a := range n.Status.Addresses {
			if a.Type == "InternalIP" {
				node.IP = a.Address
			}
		}
		snap.Nodes = append(snap.Nodes, node)
	}

	for _, p := range pods {
		pod := Pod{
			UID: p.Metadata.UID, Namespace: p.Metadata.Namespace, Name: p.Metadata.Name,
			Node: p.Spec.NodeName, IP: p.Status.PodIP, Phase: p.Status.Phase,
			Terminating: p.Metadata.DeletionTimestamp != nil, HostNetwork: p.Spec.HostNetwork,
			Labels: p.Metadata.Labels,
		}
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" {
				pod.Ready = c.Status == "True"
			}
		}
		for _, cs := range p.Status.ContainerStatuses {
			pod.Restarts += cs.RestartCount
			switch {
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
				pod.Reason = cs.State.Waiting.Reason
			case cs.State.Terminated != nil && cs.State.Terminated.Reason != "" && pod.Reason == "":
				pod.Reason = cs.State.Terminated.Reason
			case cs.LastState.Terminated != nil && cs.LastState.Terminated.Reason == "OOMKilled" && pod.Reason == "":
				pod.Reason = "OOMKilled"
			}
		}
		if len(p.Metadata.OwnerReferences) > 0 {
			o := p.Metadata.OwnerReferences[0]
			pod.Owner = o.Kind + "/" + o.Name
			if o.Kind == "ReplicaSet" { // Deployment pods: strip the hash suffix
				if i := strings.LastIndex(o.Name, "-"); i > 0 {
					pod.Owner = "Deployment/" + o.Name[:i]
				}
			}
		}
		limitsComplete := len(p.Spec.Containers) > 0
		memLimitsComplete := limitsComplete
		for _, c := range p.Spec.Containers {
			pod.CPURequestMilli += milliCPU(c.Resources.Requests["cpu"])
			pod.MemRequestBytes += bytesOf(c.Resources.Requests["memory"])
			if l := milliCPU(c.Resources.Limits["cpu"]); l > 0 {
				pod.CPULimitMilli += l
			} else {
				limitsComplete = false
			}
			if l := bytesOf(c.Resources.Limits["memory"]); l > 0 {
				pod.MemLimitBytes += l
			} else {
				memLimitsComplete = false
			}
		}
		if !limitsComplete {
			pod.CPULimitMilli = 0
		}
		if !memLimitsComplete {
			pod.MemLimitBytes = 0
		}
		snap.Pods = append(snap.Pods, pod)
	}
	sort.Slice(snap.Pods, func(i, j int) bool { return snap.Pods[i].FullName() < snap.Pods[j].FullName() })

	// EndpointSlices per service
	type epAgg struct {
		ready, total int
		readyUIDs    []string
		any          bool
	}
	agg := map[string]*epAgg{}
	for _, s := range slices_ {
		name := s.Metadata.Labels["kubernetes.io/service-name"]
		if name == "" {
			continue
		}
		key := s.Metadata.Namespace + "/" + name
		a := agg[key]
		if a == nil {
			a = &epAgg{}
			agg[key] = a
		}
		a.any = true
		for _, e := range s.Endpoints {
			a.total++
			if e.Conditions.Ready == nil || *e.Conditions.Ready { // unset means ready
				a.ready++
				if e.TargetRef != nil && e.TargetRef.Kind == "Pod" {
					a.readyUIDs = append(a.readyUIDs, e.TargetRef.UID)
				}
			}
		}
	}

	for _, s := range svcs {
		svc := Service{
			Namespace: s.Metadata.Namespace, Name: s.Metadata.Name, Type: s.Spec.Type,
			ClusterIP: s.Spec.ClusterIP, Ports: s.Spec.Ports, Selector: s.Spec.Selector,
			Probe: s.Metadata.Annotations[ProbeAnnotation], ProbePort: s.Metadata.Annotations[ProbePortAnnotation],
		}
		if len(svc.Selector) > 0 {
			for _, p := range snap.Pods {
				if p.Namespace == svc.Namespace && !p.Terminating && matches(svc.Selector, p.Labels) {
					svc.PodUIDs = append(svc.PodUIDs, p.UID)
				}
			}
		}
		if a := agg[svc.Key()]; a != nil {
			svc.HasSlices, svc.ReadyEndpoints, svc.TotalEndpoints = true, a.ready, a.total
			svc.ReadyPodUIDs = a.readyUIDs
		}
		slices.Sort(svc.PodUIDs)
		slices.Sort(svc.ReadyPodUIDs)
		snap.Services = append(snap.Services, svc)
	}
	sort.Slice(snap.Services, func(i, j int) bool { return snap.Services[i].Key() < snap.Services[j].Key() })
	return snap
}

func matches(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
