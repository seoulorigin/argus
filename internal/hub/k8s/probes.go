package k8s

import (
	"fmt"
	"strconv"
	"strings"

	"argus/internal/entities/probe"
)

const (
	// ProbeAnnotation chooses how a Service is probed: "tcp" (default), "off",
	// "http:/path" or "https:/path".
	ProbeAnnotation = "argus/probe"
	// ProbePortAnnotation picks the Service port to probe (default: the first TCP port).
	ProbePortAnnotation = "argus/probe-port"
)

// ProbeTarget describes how one Service is probed, and the key its samples count for.
type ProbeTarget struct {
	probe.Target
	Service string // "namespace/name"
}

// ProbeTargets returns what to probe for every Service that has Pods behind it
// and a cluster IP. clusterDomain is normally "cluster.local".
func (s *Snapshot) ProbeTargets(clusterDomain string) []ProbeTarget {
	if clusterDomain == "" {
		clusterDomain = "cluster.local"
	}
	var out []ProbeTarget
	for _, svc := range s.Services {
		if len(svc.Selector) == 0 || svc.ClusterIP == "" || svc.ClusterIP == "None" {
			continue
		}
		kind, path, tls, ok := parseProbeSetting(svc.Probe)
		if !ok {
			continue
		}
		port, ok := pickPort(svc)
		if !ok {
			continue
		}
		out = append(out, ProbeTarget{
			Service: svc.Key(),
			Target: probe.Target{
				ID:   fmt.Sprintf("%s:%d", svc.Key(), port),
				Kind: kind,
				Host: fmt.Sprintf("%s.%s.svc.%s", svc.Name, svc.Namespace, clusterDomain),
				Port: port, Path: path, TLS: tls,
			},
		})
	}
	return out
}

// parseProbeSetting reads the argus/probe annotation. ok is false for "off".
func parseProbeSetting(v string) (kind probe.Kind, path string, tls, ok bool) {
	v = strings.TrimSpace(v)
	switch {
	case v == "" || v == "tcp":
		return probe.TCP, "", false, true
	case v == "off" || v == "false":
		return 0, "", false, false
	case strings.HasPrefix(v, "http:"), strings.HasPrefix(v, "https:"):
		scheme, p, _ := strings.Cut(v, ":")
		if p == "" || p[0] != '/' {
			p = "/" + p
		}
		return probe.HTTP, p, scheme == "https", true
	}
	return probe.TCP, "", false, true // unknown value: fall back to the safe default
}

func pickPort(svc Service) (uint16, bool) {
	if want, err := strconv.Atoi(svc.ProbePort); err == nil {
		for _, p := range svc.Ports {
			if int(p.Port) == want && p.Protocol != "UDP" {
				return uint16(want), true
			}
		}
	}
	for _, p := range svc.Ports {
		if (p.Protocol == "" || p.Protocol == "TCP") && p.Port > 0 && p.Port < 65536 {
			return uint16(p.Port), true
		}
	}
	return 0, false
}
