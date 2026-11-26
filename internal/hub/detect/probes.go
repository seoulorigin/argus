package detect

import (
	"argus/internal/hub/k8s"
	"sort"
	"strings"
	"time"

	"argus/internal/entities/probe"
)

// probeWindow is how far back probe samples count. With a 5s probe cycle that
// is about 6 samples per node, enough to see a failure within a few cycles
// without one lost packet moving the numbers.
const probeWindow = 30 * time.Second

// minProbeSamples is the fewest samples a verdict needs.
const minProbeSamples = 3

type probeSample struct {
	at    time.Time
	node  string
	rttUs int64
	err   string
}

// probeStats summarises one service's recent probes from all nodes.
type probeStats struct {
	N        int
	Failed   int
	FailPct  float64
	P95Ms    float64 // over successful probes; 0 when none succeeded
	OKs      int
	TopError string         // most frequent failure class
	Errors   map[string]int // failures by class
	ByNode   map[string][2]int
}

// serviceOfProbe maps a probe ID "namespace/name:port" to the service key.
func serviceOfProbe(id string) string {
	if i := strings.LastIndexByte(id, ':'); i > 0 {
		return id[:i]
	}
	return id
}

// linkWindow is how far back Pod-to-Service probes count. It is shorter than
// probeWindow because a blocked connection should show within about 15 seconds.
const linkWindow = 15 * time.Second

// minLinkSamples is the fewest Pod-to-Service samples a verdict needs.
const minLinkSamples = 2

func (e *Engine) addProbeSamples(node string, samples []probe.Sample, now time.Time) {
	for _, s := range samples {
		if strings.HasPrefix(s.ID, k8s.LinkIDPrefix) {
			e.links[s.ID] = append(e.links[s.ID], probeSample{at: time.UnixMilli(s.At), node: node, rttUs: s.RTTUs, err: s.Err})
			continue
		}
		svc := serviceOfProbe(s.ID)
		e.probes[svc] = append(e.probes[svc], probeSample{at: time.UnixMilli(s.At), node: node, rttUs: s.RTTUs, err: s.Err})
	}
}

// pruneProbes drops samples older than the window and services that are gone.
func (e *Engine) pruneProbes(now time.Time, known map[string]bool) {
	linkCutoff := now.Add(-linkWindow)
	for id, list := range e.links {
		i := 0
		for i < len(list) && list[i].at.Before(linkCutoff) {
			i++
		}
		if i == len(list) {
			delete(e.links, id)
		} else {
			e.links[id] = list[i:]
		}
	}
	cutoff := now.Add(-probeWindow)
	for svc, list := range e.probes {
		if known != nil && !known[svc] {
			delete(e.probes, svc)
			continue
		}
		i := 0
		for i < len(list) && list[i].at.Before(cutoff) {
			i++
		}
		if i == len(list) {
			delete(e.probes, svc)
		} else {
			e.probes[svc] = list[i:]
		}
	}
}

func (e *Engine) probeStatsFor(svc string) (probeStats, bool) {
	list := e.probes[svc]
	if len(list) < minProbeSamples {
		return probeStats{}, false
	}
	st := probeStats{N: len(list), Errors: map[string]int{}, ByNode: map[string][2]int{}}
	var rtts []float64
	for _, s := range list {
		n := st.ByNode[s.node]
		n[0]++
		if s.err != "" {
			st.Failed++
			st.Errors[s.err]++
			n[1]++
		} else {
			rtts = append(rtts, float64(s.rttUs)/1000)
		}
		st.ByNode[s.node] = n
	}
	st.OKs = len(rtts)
	st.FailPct = float64(st.Failed) / float64(st.N) * 100
	st.P95Ms = percentile(rtts, 95)
	best := 0
	for k, v := range st.Errors {
		if v > best || (v == best && k < st.TopError) {
			best, st.TopError = v, k
		}
	}
	return st, true
}

// percentile uses the nearest-rank method: the smallest value with at least p%
// of the samples at or below it.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := int(float64(len(sorted))*p/100 + 0.999999)
	rank = max(1, min(rank, len(sorted)))
	return sorted[rank-1]
}

// linkStats summarises one Pod-to-Service check. Samples where the agent could
// not enter the Pod's namespace are counted apart: they say nothing about the
// network path, so they never count as failures.
type linkStats struct {
	N, Failed, Unusable int
	FailPct             float64
	TopError            string
	Node                string
}

func (e *Engine) linkStatsFor(id string) (linkStats, bool) {
	var st linkStats
	errs := map[string]int{}
	for _, s := range e.links[id] {
		st.Node = s.node
		switch {
		case s.err == probe.ErrNetns:
			st.Unusable++
		case s.err != "":
			st.N++
			st.Failed++
			errs[s.err]++
		default:
			st.N++
		}
	}
	if st.N < minLinkSamples {
		return st, false
	}
	st.FailPct = float64(st.Failed) / float64(st.N) * 100
	best := 0
	for k, v := range errs {
		if v > best || (v == best && k < st.TopError) {
			best, st.TopError = v, k
		}
	}
	return st, true
}
