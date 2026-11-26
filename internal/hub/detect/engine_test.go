package detect

import (
	"testing"
	"time"

	"argus/internal/entities/probe"
	"argus/internal/hub/k8s"
	"argus/internal/hub/systems"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

type sim struct {
	t      *testing.T
	e      *Engine
	now    time.Time
	events []Event
}

func newSim(t *testing.T) *sim { return &sim{t: t, e: NewEngine(DefaultConfig()), now: t0} }

// step runs one 5-second evaluation and returns the events it produced.
func (s *sim) step(snap *k8s.Snapshot, nodes ...systems.NodeObservation) []Event {
	ev := s.e.Evaluate(Inputs{Now: s.now, K8s: snap, Nodes: nodes})
	s.now = s.now.Add(5 * time.Second)
	s.events = append(s.events, ev...)
	return ev
}

func kinds(ev []Event) []string {
	var out []string
	for _, e := range ev {
		out = append(out, string(e.Kind)+":"+e.Incident.Rule)
	}
	return out
}

func cartSnapshot(ready int) *k8s.Snapshot {
	return &k8s.Snapshot{
		Nodes: []k8s.Node{{Name: "n1", Ready: true, CPUMilli: 4000}, {Name: "n2", Ready: true, CPUMilli: 4000}},
		Pods: []k8s.Pod{
			{UID: "u1", Namespace: "shop", Name: "cart-1", Node: "n1", Phase: "Running", Ready: true, MemLimitBytes: 100 << 20, CPULimitMilli: 500},
			{UID: "u2", Namespace: "shop", Name: "cart-2", Node: "n2", Phase: "Running", Ready: ready > 1},
		},
		Services: []k8s.Service{{Namespace: "shop", Name: "cart", Selector: map[string]string{"app": "cart"},
			PodUIDs: []string{"u1", "u2"}, HasSlices: true, ReadyEndpoints: ready, TotalEndpoints: 2}},
	}
}

func svc(v View, key string) ServiceStatus {
	for _, s := range v.Services {
		if s.Key == key {
			return s
		}
	}
	return ServiceStatus{}
}

func up(name string, cpu, mem float64, pods ...systems.PodUsage) systems.NodeObservation {
	return systems.NodeObservation{Name: name, Status: "up", Last: &systems.Observation{At: t0, CPUPct: cpu, MemPct: mem, Pods: pods}}
}

func TestServiceDownNeedsThreeConsecutiveChecksAndThreeToRecover(t *testing.T) {
	s := newSim(t)
	assert.Empty(t, s.step(cartSnapshot(2), up("n1", 10, 10), up("n2", 10, 10)))
	assert.Equal(t, Up, svc(s.e.View(), "shop/cart").Status)

	assert.Empty(t, s.step(cartSnapshot(0)), "1st bad check")
	assert.Empty(t, s.step(cartSnapshot(0)), "2nd bad check")
	assert.Equal(t, Up, svc(s.e.View(), "shop/cart").Status, "not declared yet")

	ev := s.step(cartSnapshot(0)) // 3rd
	require.Equal(t, []string{"opened:service_down"}, kinds(ev))
	inc := ev[0].Incident
	assert.Equal(t, Critical, inc.Severity)
	assert.Equal(t, "shop/cart", inc.Target)
	assert.Equal(t, "ready endpoints = 0", inc.Threshold)
	assert.Equal(t, "ready endpoints 0 of 2", inc.Observed)
	assert.Equal(t, t0.Add(5*time.Second), inc.StartedAt, "starts at the first violating check")
	assert.Equal(t, "3 of last 5 checks (every 5s)", inc.Window)
	v := s.e.View()
	assert.Equal(t, Down, svc(v, "shop/cart").Status)
	require.Len(t, v.Open, 1)

	assert.Empty(t, s.step(cartSnapshot(2)), "recovering: 1")
	assert.Empty(t, s.step(cartSnapshot(2)), "recovering: 2")
	assert.Equal(t, Down, svc(s.e.View(), "shop/cart").Status)
	ev = s.step(cartSnapshot(2)) // 3rd healthy
	require.Equal(t, []string{"recovered:service_down"}, kinds(ev))
	require.NotNil(t, ev[0].Incident.RecoveredAt)
	assert.False(t, ev[0].Incident.Open)
	assert.Equal(t, Up, svc(s.e.View(), "shop/cart").Status)
	assert.Len(t, s.e.View().Recent, 1)
}

func TestFlappingBelowThreeOfFiveDoesNotOpen(t *testing.T) {
	s := newSim(t)
	for _, ready := range []int{0, 2, 0, 2} {
		assert.Empty(t, s.step(cartSnapshot(ready)))
	}
	// the fifth check makes it 3 violations in the last 5 -> opens
	assert.Equal(t, []string{"opened:service_down"}, kinds(s.step(cartSnapshot(0))))
}

func TestPartialOutageIsWarn(t *testing.T) {
	s := newSim(t)
	for range 3 {
		s.step(cartSnapshot(1))
	}
	got := svc(s.e.View(), "shop/cart")
	assert.Equal(t, Warn, got.Status)
	assert.Contains(t, got.Reason, "ready endpoints 1 of 2")
	assert.Equal(t, []string{"opened:service_degraded"}, kinds(s.events))
}

func TestPodMemoryHysteresis(t *testing.T) {
	s := newSim(t)
	pod := func(memMiB uint64) systems.NodeObservation {
		return up("n1", 10, 10, systems.PodUsage{UID: "u1", MemBytes: memMiB << 20})
	}
	for range 2 {
		s.step(cartSnapshot(2), pod(95)) // 95% of 100 MiB limit, threshold 90%
	}
	assert.Equal(t, Up, svc(s.e.View(), "shop/cart").Status)
	ev := s.step(cartSnapshot(2), pod(95))
	require.Equal(t, []string{"opened:pod_memory_high"}, kinds(ev))
	assert.Equal(t, Warning, ev[0].Incident.Severity)
	assert.Equal(t, "shop/cart-1", ev[0].Incident.Target)
	assert.Equal(t, "memory > 90% of limit (100 MiB)", ev[0].Incident.Threshold)
	assert.Equal(t, Warn, svc(s.e.View(), "shop/cart").Status, "a Pod problem makes its service WARN")
	assert.Contains(t, svc(s.e.View(), "shop/cart").Reason, "shop/cart-1")

	// 85% is below the 90% threshold but above 72% (= 0.8 x 90): not recovered
	for range 5 {
		assert.Empty(t, s.step(cartSnapshot(2), pod(85)))
	}
	assert.Equal(t, Warn, svc(s.e.View(), "shop/cart").Status)
	// 70% three times in a row recovers
	s.step(cartSnapshot(2), pod(70))
	s.step(cartSnapshot(2), pod(70))
	assert.Equal(t, []string{"recovered:pod_memory_high"}, kinds(s.step(cartSnapshot(2), pod(70))))
	assert.Equal(t, Up, svc(s.e.View(), "shop/cart").Status)
}

func TestPodCPUUsesNodeCapacityAndLimit(t *testing.T) {
	s := newSim(t)
	// 4000m node, limit 500m. 12.5% of the node = 500m = 100% of the limit.
	busy := up("n1", 20, 10, systems.PodUsage{UID: "u1", CPUPct: 12.5})
	for range 3 {
		s.step(cartSnapshot(2), busy)
	}
	require.Equal(t, []string{"opened:pod_cpu_high"}, kinds(s.events))
	assert.InDelta(t, 100.0, s.events[0].Incident.Peak, 0.01)
}

func TestNodeDownIsImmediateAndNamesAffectedService(t *testing.T) {
	s := newSim(t)
	s.step(cartSnapshot(2), up("n1", 10, 10), up("n2", 10, 10))
	down := systems.NodeObservation{Name: "n2", Status: "down"}
	ev := s.step(cartSnapshot(1), up("n1", 10, 10), down)
	require.Contains(t, kinds(ev), "opened:node_down", "the poller already waited 3 polls; no second delay")
	assert.Equal(t, Down, func() Status {
		for _, n := range s.e.View().Nodes {
			if n.Name == "n2" {
				return n.Status
			}
		}
		return ""
	}())
	// the service is degraded after its own 3 checks, and the incident names the dead node
	s.step(cartSnapshot(1), up("n1", 10, 10), down)
	ev = s.step(cartSnapshot(1), up("n1", 10, 10), down)
	require.Contains(t, kinds(ev), "opened:service_degraded")
	for _, e := range ev {
		if e.Incident.Rule == RuleServiceDegraded {
			assert.Equal(t, []string{"n2"}, e.Incident.Detail["downNodes"])
		}
	}
	// back up: 2 healthy checks close the node incident
	s.step(cartSnapshot(2), up("n1", 10, 10), up("n2", 10, 10))
	ev = s.step(cartSnapshot(2), up("n1", 10, 10), up("n2", 10, 10))
	assert.Contains(t, kinds(ev), "recovered:node_down")
}

func TestKubeletNotReadyOpensNodeDown(t *testing.T) {
	s := newSim(t)
	snap := cartSnapshot(2)
	snap.Nodes[1].Ready = false
	ev := s.step(snap, up("n1", 10, 10), up("n2", 10, 10))
	require.Contains(t, kinds(ev), "opened:node_down")
	assert.Equal(t, "kubelet NotReady", ev[0].Incident.Observed)
}

func TestKubernetesOutageKeepsOpenIncidents(t *testing.T) {
	s := newSim(t)
	for range 3 {
		s.step(cartSnapshot(0))
	}
	require.Len(t, s.e.View().Open, 1)
	for range 5 {
		assert.Empty(t, s.step(nil), "no cluster view: nothing opens or closes")
	}
	assert.Len(t, s.e.View().Open, 1, "the incident stays open through an API outage")
}

func TestRemovedTargetClosesItsIncident(t *testing.T) {
	s := newSim(t)
	for range 3 {
		s.step(cartSnapshot(0))
	}
	snap := cartSnapshot(0)
	snap.Services = nil
	ev := s.step(snap)
	require.Equal(t, []string{"recovered:service_down"}, kinds(ev))
	assert.Equal(t, "target no longer exists", ev[0].Incident.Note)
}

func TestServiceWithoutEndpointSliceIsUnknownNotDown(t *testing.T) {
	s := newSim(t)
	snap := cartSnapshot(0)
	snap.Services[0].HasSlices = false
	for range 6 {
		assert.Empty(t, s.step(snap))
	}
	assert.Equal(t, Unknown, svc(s.e.View(), "shop/cart").Status)
}

func TestCrashLoopOpensPodUnhealthy(t *testing.T) {
	s := newSim(t)
	snap := cartSnapshot(2)
	snap.Pods[0].Reason, snap.Pods[0].Restarts, snap.Pods[0].Ready = "CrashLoopBackOff", 7, false
	for range 3 {
		s.step(snap)
	}
	require.Equal(t, []string{"opened:pod_unhealthy"}, kinds(s.events))
	assert.Equal(t, Warn, svc(s.e.View(), "shop/cart").Status)
}

func TestServiceUsageSumsPodsAndOmitsPartialLimits(t *testing.T) {
	s := newSim(t)
	snap := cartSnapshot(2)
	// u1 has limits (500m / 100Mi), u2 has none, so the totals must not claim a limit
	s.step(snap, up("n1", 10, 10,
		systems.PodUsage{UID: "u1", CPUPct: 5, MemBytes: 40 << 20},
		systems.PodUsage{UID: "u2", CPUPct: 2.5, MemBytes: 10 << 20}))
	got := svc(s.e.View(), "shop/cart")
	assert.InDelta(t, 300.0, got.CPUMilli, 0.01, "(5%% + 2.5%%) of a 4000m node") // 200m + 100m
	assert.EqualValues(t, 50<<20, got.MemBytes)
	assert.Zero(t, got.CPULimitMilli)
	assert.Zero(t, got.MemLimitBytes)

	snap.Pods[1].CPULimitMilli, snap.Pods[1].MemLimitBytes = 500, 100<<20
	s.step(snap)
	got = svc(s.e.View(), "shop/cart")
	assert.EqualValues(t, 1000, got.CPULimitMilli)
	assert.EqualValues(t, 200<<20, got.MemLimitBytes)
}

func TestHistoryKeepsFifteenMinutesAndForgetsRemovedTargets(t *testing.T) {
	s := newSim(t)
	for range 200 {
		s.step(cartSnapshot(2), up("n1", 10, 20))
	}
	h := s.e.History("service/shop/cart")
	require.Len(t, h, 180, "180 samples x 5s = 15 minutes")
	assert.Equal(t, int64(5000), h[1].T-h[0].T)
	assert.Len(t, s.e.History("node/n1"), 180)
	assert.InDelta(t, 20.0, s.e.History("node/n1")[0].M, 0.001)

	snap := cartSnapshot(2)
	snap.Services = nil
	s.step(snap, up("n1", 10, 20))
	assert.Empty(t, s.e.History("service/shop/cart"))
}

// probeStep feeds one 5s cycle of probe samples (one per node) and evaluates.
func (s *sim) probeStep(snap *k8s.Snapshot, rttMs map[string]float64, errs map[string]string) []Event {
	var nodes []systems.NodeObservation
	for _, n := range []string{"n1", "n2", "n3"} {
		o := up(n, 10, 10)
		smp := probe.Sample{ID: "shop/cart:7070", At: s.now.UnixMilli(), RTTUs: int64(rttMs[n] * 1000), Err: errs[n]}
		o.Probes = []probe.Sample{smp}
		nodes = append(nodes, o)
	}
	return s.step(snap, nodes...)
}

func all(v float64) map[string]float64  { return map[string]float64{"n1": v, "n2": v, "n3": v} }
func allErr(e string) map[string]string { return map[string]string{"n1": e, "n2": e, "n3": e} }

func TestHealthyProbesOpenNothing(t *testing.T) {
	s := newSim(t)
	for range 10 {
		s.probeStep(cartSnapshot(2), all(20), nil)
	}
	assert.Empty(t, s.events)
	got := svc(s.e.View(), "shop/cart")
	assert.Equal(t, Up, got.Status)
	assert.InDelta(t, 20.0, got.P95Ms, 0.01)
	assert.Zero(t, got.FailPct)
	assert.Positive(t, got.ProbeN)
}

func TestAllProbesTimingOutMakesServiceUnreachable(t *testing.T) {
	s := newSim(t)
	for range 6 { // warm window of good samples
		s.probeStep(cartSnapshot(2), all(20), nil)
	}
	var opened *Event
	steps := 0
	for ; steps < 12 && opened == nil; steps++ {
		for _, ev := range s.probeStep(cartSnapshot(2), nil, allErr("timeout")) {
			if ev.Incident.Rule == RuleUnreachable && ev.Kind == Opened {
				e := ev
				opened = &e
			}
		}
	}
	require.NotNil(t, opened, "failure must be detected")
	assert.GreaterOrEqual(t, steps, 4, "the 30s window and the 3-of-5 rule keep one bad cycle from firing")
	assert.LessOrEqual(t, steps, 8)
	assert.Equal(t, Critical, opened.Incident.Severity)
	assert.Contains(t, opened.Incident.Observed, "timeout")
	assert.Equal(t, Down, svc(s.e.View(), "shop/cart").Status)
	assert.Equal(t, "timeout", svc(s.e.View(), "shop/cart").ProbeError)
	assert.NotNil(t, opened.Incident.Detail["byNode"])

	// recovery: good probes flush the window, then 3 healthy evaluations close it
	recovered := false
	for range 14 {
		for _, ev := range s.probeStep(cartSnapshot(2), all(20), nil) {
			if ev.Incident.Rule == RuleUnreachable && ev.Kind == Recovered {
				recovered = true
			}
		}
	}
	assert.True(t, recovered)
	assert.Equal(t, Up, svc(s.e.View(), "shop/cart").Status)
}

func TestSlowResponsesOpenLatencyWarning(t *testing.T) {
	s := newSim(t)
	for range 8 {
		s.probeStep(cartSnapshot(2), all(400), nil)
	}
	var rules []string
	for _, e := range s.events {
		rules = append(rules, string(e.Kind)+":"+e.Incident.Rule)
	}
	assert.Equal(t, []string{"opened:service_latency_high"}, rules)
	got := svc(s.e.View(), "shop/cart")
	assert.Equal(t, Warn, got.Status)
	assert.InDelta(t, 400.0, got.P95Ms, 0.01)
	assert.Contains(t, s.events[0].Incident.Threshold, "250 ms")
}

func TestOneNodeFailingIsWarningNotDown(t *testing.T) {
	s := newSim(t)
	for range 12 {
		s.probeStep(cartSnapshot(2), all(20), map[string]string{"n3": "refused"}) // 1 of 3 vantage points: 33%
	}
	var rules []string
	for _, e := range s.events {
		rules = append(rules, string(e.Kind)+":"+e.Incident.Rule)
	}
	assert.Equal(t, []string{"opened:service_errors"}, rules)
	assert.Equal(t, Warn, svc(s.e.View(), "shop/cart").Status)
	byNode := s.events[0].Incident.Detail["byNode"].(map[string]string)
	assert.Contains(t, byNode["n1"], "0/")
	assert.Contains(t, byNode["n3"], "failed")
}

func TestProbeRulesStayQuietWhenNoEndpointIsReady(t *testing.T) {
	s := newSim(t)
	for range 12 {
		s.probeStep(cartSnapshot(0), nil, allErr("refused"))
	}
	var rules []string
	for _, e := range s.events {
		rules = append(rules, e.Incident.Rule)
	}
	assert.Equal(t, []string{RuleServiceDown}, rules, "one incident for one cause")
}

func TestPercentileNearestRank(t *testing.T) {
	ten := []float64{5, 1, 9, 3, 7, 2, 10, 4, 8, 6}
	assert.Equal(t, 10.0, percentile(ten, 95))
	assert.Equal(t, 5.0, percentile(ten, 50))
	assert.Equal(t, 2.0, percentile([]float64{1, 2, 3, 4}, 50))
	assert.Zero(t, percentile(nil, 95))
}

func linkSnapshot() *k8s.Snapshot {
	snap := cartSnapshot(2)
	snap.Services = append(snap.Services, k8s.Service{Namespace: "shop", Name: "web", Selector: map[string]string{"app": "web"},
		PodUIDs: []string{"w1"}, HasSlices: true, ReadyEndpoints: 1, TotalEndpoints: 1})
	snap.Pods = append(snap.Pods, k8s.Pod{UID: "w1", Namespace: "shop", Name: "web-1", Node: "n1", Phase: "Running", Ready: true})
	return snap
}

var webToCart = []k8s.LinkTarget{{SrcPodName: "shop/web-1", SrcService: "shop/web", DstService: "shop/cart",
	Target: probe.Target{ID: "link|shop/web-1|shop/cart:7070"}}}

// linkStep gives one cycle: the node-origin probes of "cart" are healthy, while the
// probe from inside web-1 to cart is reported with linkErr ("" = ok).
func (s *sim) linkStep(linkErr string) []Event {
	nodes := []systems.NodeObservation{up("n1", 10, 10), up("n2", 10, 10), up("n3", 10, 10)}
	for i := range nodes {
		nodes[i].Probes = []probe.Sample{{ID: "shop/cart:7070", At: s.now.UnixMilli(), RTTUs: 1000}}
	}
	nodes[0].Probes = append(nodes[0].Probes, probe.Sample{ID: "link|shop/web-1|shop/cart:7070", At: s.now.UnixMilli(), RTTUs: 1500, Err: linkErr})
	ev := s.e.Evaluate(Inputs{Now: s.now, K8s: linkSnapshot(), Nodes: nodes, Links: webToCart})
	s.now = s.now.Add(5 * time.Second)
	s.events = append(s.events, ev...)
	return ev
}

func TestBlockedPodToServiceLinkIsDetectedWithinAboutFifteenSeconds(t *testing.T) {
	s := newSim(t)
	for range 3 {
		assert.Empty(t, s.linkStep(""))
	}
	failStart := s.now
	var opened *Event
	var openedAfter time.Duration
	for i := 0; i < 6 && opened == nil; i++ {
		for _, ev := range s.linkStep("timeout") {
			if ev.Incident.Rule == RuleLinkFailed && ev.Kind == Opened {
				e := ev
				opened = &e
				openedAfter = s.now.Add(-5*time.Second).Sub(failStart) + 5*time.Second
			}
		}
	}
	require.NotNil(t, opened)
	assert.LessOrEqual(t, openedAfter, 20*time.Second, "blocked connection must show quickly")
	inc := opened.Incident
	assert.Equal(t, "shop/web-1 → shop/cart", inc.Target)
	assert.Contains(t, inc.Observed, "timeout")
	assert.Equal(t, "shop/web", inc.Detail["fromService"])
	assert.EqualValues(t, 0, inc.Detail["destinationFailPctFromNodes"], "the destination is fine when probed from the nodes")

	v := s.e.View()
	assert.Equal(t, Warn, svc(v, "shop/web").Status, "the caller is affected")
	assert.Contains(t, svc(v, "shop/web").Reason, "shop/web-1 → shop/cart")
	assert.Equal(t, Up, svc(v, "shop/cart").Status, "the callee is healthy: only the link is cut")

	// the policy is removed: three healthy checks close the incident
	var recovered bool
	for range 6 {
		for _, ev := range s.linkStep("") {
			if ev.Kind == Recovered && ev.Incident.Rule == RuleLinkFailed {
				recovered = true
			}
		}
	}
	assert.True(t, recovered)
	assert.Equal(t, Up, svc(s.e.View(), "shop/web").Status)
}

func TestAgentThatCannotEnterThePodNeverBlamesTheNetwork(t *testing.T) {
	s := newSim(t)
	for range 10 {
		s.linkStep(probe.ErrNetns)
	}
	assert.Empty(t, s.events, "a missing capability is not a blocked link")
	assert.Equal(t, Up, svc(s.e.View(), "shop/web").Status)
}

func TestLinkIncidentStaysOpenWhenAgentGoesQuiet(t *testing.T) {
	s := newSim(t)
	for range 4 {
		s.linkStep("refused")
	}
	require.Len(t, s.e.View().Open, 1)
	// the agent stops reporting; the link is still configured
	for range 5 {
		s.e.Evaluate(Inputs{Now: s.now, K8s: linkSnapshot(), Nodes: []systems.NodeObservation{up("n1", 1, 1)}, Links: webToCart})
		s.now = s.now.Add(5 * time.Second)
	}
	assert.Len(t, s.e.View().Open, 1, "no data is not recovery")
}
