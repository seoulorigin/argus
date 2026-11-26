package k8s

import (
	"fmt"
	"strconv"
	"strings"

	"argus/internal/entities/probe"
)

// LinkPair names a caller and a callee Service whose connection is checked from
// inside the caller's Pods. Port 0 means the callee's default probe port.
type LinkPair struct {
	From string // "namespace/service"
	To   string // "namespace/service"
	Port uint16
}

// ParseLinkPairs reads "ns/frontend>ns/cart[:port],ns/checkout>ns/payment". An
// empty string is a valid, empty list.
func ParseLinkPairs(spec string) ([]LinkPair, error) {
	var out []LinkPair
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		from, to, ok := strings.Cut(item, ">")
		if !ok {
			return nil, fmt.Errorf("pod probe %q: expected from>to", item)
		}
		pair := LinkPair{From: strings.TrimSpace(from), To: strings.TrimSpace(to)}
		if host, port, hasPort := strings.Cut(pair.To, ":"); hasPort {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("pod probe %q: bad port %q", item, port)
			}
			pair.To, pair.Port = host, uint16(n)
		}
		for _, side := range []string{pair.From, pair.To} {
			if ns, name, ok := strings.Cut(side, "/"); !ok || ns == "" || name == "" || strings.ContainsAny(name, "/:>") {
				return nil, fmt.Errorf("pod probe %q: %q must be namespace/service", item, side)
			}
		}
		out = append(out, pair)
	}
	return out, nil
}

// LinkTarget is one source Pod's connection check towards a Service.
type LinkTarget struct {
	probe.Target
	SrcPodName string // "namespace/name"
	SrcService string // "namespace/service"
	DstService string // "namespace/service"
}

// LinkIDPrefix starts the ID of every Pod-to-Service probe, followed by
// "<source pod>|<destination service>:<port>".
const LinkIDPrefix = "link|"

// LinkTargets expands the configured pairs into one check per running source
// Pod. The destination is its ClusterIP, so the check needs no DNS from inside
// the Pod and stays a statement about the network path.
func (s *Snapshot) LinkTargets(pairs []LinkPair) []LinkTarget {
	svcs := make(map[string]Service, len(s.Services))
	for _, svc := range s.Services {
		svcs[svc.Key()] = svc
	}
	pods := s.PodByUID()

	var out []LinkTarget
	for _, pair := range pairs {
		from, okFrom := svcs[pair.From]
		to, okTo := svcs[pair.To]
		if !okFrom || !okTo || to.ClusterIP == "" || to.ClusterIP == "None" {
			continue
		}
		kind, path, tls, _ := parseProbeSetting(to.Probe) // the link is checked even if the Service's own probe is off
		port := pair.Port
		if port == 0 {
			p, ok := pickPort(to)
			if !ok {
				continue
			}
			port = p
		}
		for _, uid := range from.PodUIDs {
			p := pods[uid]
			if p == nil || p.HostNetwork || p.Terminating || p.Phase != "Running" {
				continue
			}
			out = append(out, LinkTarget{
				Target: probe.Target{
					ID:   fmt.Sprintf("%s%s|%s:%d", LinkIDPrefix, p.FullName(), to.Key(), port),
					Kind: kind, Host: to.ClusterIP, Port: port, Path: path, TLS: tls, SrcPod: uid,
				},
				SrcPodName: p.FullName(), SrcService: from.Key(), DstService: to.Key(),
			})
		}
	}
	return out
}
