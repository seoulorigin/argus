package detect

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"argus/internal/hub/k8s"
	"argus/internal/hub/systems"
)

// Inputs is everything one evaluation looks at.
type Inputs struct {
	Now   time.Time
	K8s   *k8s.Snapshot // nil until the cluster view has been read once
	Nodes []systems.NodeObservation
	// Links are the Pod-to-Service checks currently configured.
	Links []k8s.LinkTarget
}

// ruleParams says how many recent evaluations a rule looks at.
type ruleParams struct{ window, trigger, recover int }

var defaultParams = ruleParams{window: 5, trigger: 3, recover: 3}

// A node is declared down by the poller only after 3 failed polls, so that rule
// reacts at once and only waits for 2 healthy polls to clear.
var paramsFor = map[string]ruleParams{
	RuleNodeDown: {window: 1, trigger: 1, recover: 2},
	// A blocked Pod-to-Service connection must show within about 15 seconds.
	RuleLinkFailed: {window: 3, trigger: 2, recover: 3},
}

func params(rule string) ruleParams {
	if p, ok := paramsFor[rule]; ok {
		return p
	}
	return defaultParams
}

const (
	sourceK8s   = "k8s"
	sourceNodes = "nodes"
	recentLimit = 100
)

type key struct{ rule, target string }

// observation is one rule's verdict about one target in one evaluation.
type observation struct {
	key        key
	source     string
	targetType string
	severity   Severity
	violated   bool // counts toward opening
	healthy    bool // counts toward closing; neither violated nor healthy is the dead band
	value      float64
	threshold  string
	observed   string
	detail     map[string]any
}

type entry struct {
	violated bool
	at       time.Time
}

type ruleState struct {
	ring     []entry
	okStreak int
	incident *Incident
	source   string
}

// Engine evaluates the rules. It is safe for one goroutine to call Evaluate
// while others call View.
type Engine struct {
	cfg Config

	mu     sync.Mutex
	states map[key]*ruleState
	recent []Incident
	view   View
	hist   map[string][]Point       // "service/<ns>/<name>" and "node/<name>"
	probes map[string][]probeSample // recent probe samples per service key
	links  map[string][]probeSample // recent Pod-to-Service samples per link ID
}

// historyLen is 15 minutes of 5-second samples.
const historyLen = 180

// History returns up to 15 minutes of samples for "service/<ns>/<name>" or "node/<name>".
func (e *Engine) History(key string) []Point {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Point(nil), e.hist[key]...)
}

func (e *Engine) record(key string, p Point, live map[string]bool) {
	live[key] = true
	h := append(e.hist[key], p)
	if len(h) > historyLen {
		h = h[len(h)-historyLen:]
	}
	e.hist[key] = h
}

func NewEngine(cfg Config) *Engine {
	return &Engine{cfg: cfg, states: map[key]*ruleState{}, hist: map[string][]Point{}, probes: map[string][]probeSample{}, links: map[string][]probeSample{}}
}

// View returns the picture from the latest evaluation.
func (e *Engine) View() View {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.view
}

// Evaluate runs every rule once and returns the incident changes it caused.
func (e *Engine) Evaluate(in Inputs) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, n := range in.Nodes {
		e.addProbeSamples(n.Name, n.Probes, in.Now)
	}
	var known map[string]bool
	if in.K8s != nil {
		known = make(map[string]bool, len(in.K8s.Services))
		for _, s := range in.K8s.Services {
			known[s.Key()] = true
		}
	}
	e.pruneProbes(in.Now, known)

	obs := e.observe(in)
	seen := make(map[key]bool, len(obs))
	var events []Event

	for _, o := range obs {
		seen[o.key] = true
		st := e.states[o.key]
		if st == nil {
			st = &ruleState{source: o.source}
			e.states[o.key] = st
		}
		p := params(o.key.rule)
		st.ring = append(st.ring, entry{violated: o.violated, at: in.Now})
		if len(st.ring) > p.window {
			st.ring = st.ring[len(st.ring)-p.window:]
		}
		switch {
		case o.healthy:
			st.okStreak++
		default:
			st.okStreak = 0
		}

		if st.incident == nil {
			count, first := 0, time.Time{}
			for _, en := range st.ring {
				if en.violated {
					if count == 0 {
						first = en.at
					}
					count++
				}
			}
			if count >= p.trigger {
				inc := &Incident{
					ID:         fmt.Sprintf("%s|%s|%d", o.key.rule, o.key.target, first.UnixMilli()),
					Rule:       o.key.rule,
					TargetType: o.targetType,
					Target:     o.key.target,
					Severity:   o.severity,
					Open:       true,
					Threshold:  o.threshold,
					Observed:   o.observed,
					Peak:       o.value,
					Window:     windowText(p),
					StartedAt:  first,
					Detail:     o.detail,
				}
				st.incident = inc
				events = append(events, Event{Opened, *inc})
			}
			continue
		}

		inc := st.incident
		if o.violated {
			inc.Observed = o.observed
			if o.value > inc.Peak {
				inc.Peak = o.value
				events = append(events, Event{Updated, *inc})
			}
		}
		if st.okStreak >= p.recover {
			events = append(events, e.recover(st, in.Now, ""))
			st.ring = nil
			st.okStreak = 0
		}
	}

	// Targets that vanished (pod deleted, service removed, node gone). Only the
	// sources that were actually evaluated this round are swept, so a Kubernetes
	// outage cannot be mistaken for "everything was removed".
	evaluated := map[string]bool{sourceNodes: true}
	if in.K8s != nil {
		evaluated[sourceK8s] = true
	}
	for k, st := range e.states {
		if seen[k] || !evaluated[st.source] {
			continue
		}
		if st.incident != nil {
			events = append(events, e.recover(st, in.Now, "target no longer exists"))
		}
		delete(e.states, k)
	}

	e.view = e.buildView(in)
	return events
}

func (e *Engine) recover(st *ruleState, now time.Time, note string) Event {
	inc := st.incident
	inc.Open = false
	t := now
	inc.RecoveredAt = &t
	inc.Note = note
	ev := Event{Recovered, *inc}
	e.recent = append([]Incident{*inc}, e.recent...)
	if len(e.recent) > recentLimit {
		e.recent = e.recent[:recentLimit]
	}
	st.incident = nil
	return ev
}

func windowText(p ruleParams) string {
	if p.window == 1 {
		return "immediate (every 5s)"
	}
	return fmt.Sprintf("%d of last %d checks (every 5s)", p.trigger, p.window)
}

// threshold helpers: above is violated, at or below hysteresis*threshold is healthy.
func (e *Engine) level(value, threshold float64) (violated, healthy bool) {
	return value > threshold, value <= threshold*e.cfg.Hysteresis
}

func (e *Engine) observe(in Inputs) []observation {
	var out []observation
	out = append(out, e.observeNodes(in)...)
	if in.K8s != nil {
		out = append(out, e.observeServices(in)...)
		out = append(out, e.observePods(in)...)
		out = append(out, e.observeLinks(in)...)
	}
	return out
}

func (e *Engine) observeNodes(in Inputs) []observation {
	type nodeView struct {
		agent *systems.NodeObservation
		kube  *k8s.Node
	}
	nodes := map[string]*nodeView{}
	for i := range in.Nodes {
		if n := in.Nodes[i]; n.Name != "" {
			nodes[n.Name] = &nodeView{agent: &in.Nodes[i]}
		}
	}
	if in.K8s != nil {
		for i := range in.K8s.Nodes {
			n := &in.K8s.Nodes[i]
			v := nodes[n.Name]
			if v == nil {
				v = &nodeView{}
				nodes[n.Name] = v
			}
			v.kube = n
		}
	}

	var out []observation
	for name, v := range nodes {
		// node_down: the agent stopped answering, or the kubelet reports NotReady.
		agentDown := v.agent != nil && v.agent.Status == "down"
		agentUp := v.agent == nil || v.agent.Status == "up"
		kubeDown := v.kube != nil && !v.kube.Ready
		kubeUp := v.kube == nil || v.kube.Ready
		var why []string
		if agentDown {
			why = append(why, "agent not responding")
		}
		if kubeDown {
			why = append(why, "kubelet NotReady")
		}
		out = append(out, observation{
			key: key{RuleNodeDown, name}, source: sourceNodes, targetType: TargetNode, severity: Critical,
			violated: agentDown || kubeDown, healthy: agentUp && kubeUp,
			value: b2f(agentDown || kubeDown), threshold: "node responds and is Ready",
			observed: strings.Join(why, ", "),
		})

		if v.agent != nil && v.agent.Status == "up" && v.agent.Last != nil {
			violated, healthy := e.level(v.agent.Last.CPUPct, e.cfg.NodeCPUPct)
			out = append(out, observation{
				key: key{RuleNodeCPU, name}, source: sourceNodes, targetType: TargetNode, severity: Warning,
				violated: violated, healthy: healthy, value: v.agent.Last.CPUPct,
				threshold: fmt.Sprintf("CPU > %.0f%%", e.cfg.NodeCPUPct), observed: fmt.Sprintf("CPU %.1f%%", v.agent.Last.CPUPct),
			})
			violated, healthy = e.level(v.agent.Last.MemPct, e.cfg.NodeMemPct)
			out = append(out, observation{
				key: key{RuleNodeMemory, name}, source: sourceNodes, targetType: TargetNode, severity: Warning,
				violated: violated, healthy: healthy, value: v.agent.Last.MemPct,
				threshold: fmt.Sprintf("memory > %.0f%%", e.cfg.NodeMemPct), observed: fmt.Sprintf("memory %.1f%%", v.agent.Last.MemPct),
			})
		}
		if v.kube != nil {
			out = append(out, observation{
				key: key{RuleNodePressure, name}, source: sourceNodes, targetType: TargetNode, severity: Warning,
				violated: v.kube.ConditionsDown != "", healthy: v.kube.ConditionsDown == "",
				value: b2f(v.kube.ConditionsDown != ""), threshold: "no pressure condition (disk, memory, PID)",
				observed: v.kube.ConditionsDown,
			})
		}
	}
	return out
}

func (e *Engine) observeServices(in Inputs) []observation {
	pods := in.K8s.PodByUID()
	downNodes := map[string]bool{}
	for _, n := range in.K8s.Nodes {
		if !n.Ready {
			downNodes[n.Name] = true
		}
	}
	for _, n := range in.Nodes {
		if n.Status == "down" {
			downNodes[n.Name] = true
		}
	}

	var out []observation
	for _, s := range in.K8s.Services {
		// A Service with no selector has no Pods to count, and one with no
		// EndpointSlice yet gives no evidence either way.
		if len(s.Selector) == 0 || !s.HasSlices {
			continue
		}
		detail := map[string]any{"ready": s.ReadyEndpoints, "total": s.TotalEndpoints}
		var podInfo []map[string]any
		var affected []string
		for _, uid := range s.PodUIDs {
			if p := pods[uid]; p != nil {
				podInfo = append(podInfo, map[string]any{"name": p.FullName(), "node": p.Node, "ready": p.Ready, "reason": p.Reason})
				if downNodes[p.Node] {
					affected = append(affected, p.Node)
				}
			}
		}
		detail["pods"] = podInfo
		if len(affected) > 0 {
			sort.Strings(affected)
			detail["downNodes"] = dedupe(affected)
		}

		down := s.ReadyEndpoints == 0
		out = append(out, observation{
			key: key{RuleServiceDown, s.Key()}, source: sourceK8s, targetType: TargetService, severity: Critical,
			violated: down, healthy: !down, value: float64(s.TotalEndpoints - s.ReadyEndpoints),
			threshold: "ready endpoints = 0", observed: fmt.Sprintf("ready endpoints %d of %d", s.ReadyEndpoints, s.TotalEndpoints),
			detail: detail,
		})
		degraded := s.ReadyEndpoints > 0 && s.ReadyEndpoints < s.TotalEndpoints
		out = append(out, observation{
			key: key{RuleServiceDegraded, s.Key()}, source: sourceK8s, targetType: TargetService, severity: Warning,
			violated: degraded, healthy: s.ReadyEndpoints == s.TotalEndpoints && s.TotalEndpoints > 0,
			value:     float64(s.TotalEndpoints - s.ReadyEndpoints),
			threshold: "ready endpoints < total", observed: fmt.Sprintf("ready endpoints %d of %d", s.ReadyEndpoints, s.TotalEndpoints),
			detail: detail,
		})
		out = append(out, e.observeProbes(s, detail)...)
	}
	return out
}

// observeProbes judges a service by what the probes saw. It always returns the
// three probe observations; without enough samples they are neutral, so the
// absence of data neither opens nor closes anything. When the service has no
// ready endpoints, "service_down" already says why probes fail, so the probe
// rules stay neutral instead of raising a second incident for the same cause.
func (e *Engine) observeProbes(s k8s.Service, base map[string]any) []observation {
	svc := s.Key()
	mk := func(rule string, sev Severity) observation {
		return observation{key: key{rule, svc}, source: sourceK8s, targetType: TargetService, severity: sev}
	}
	unreachable, errs, latency := mk(RuleUnreachable, Critical), mk(RuleProbeErrors, Warning), mk(RuleLatency, Warning)
	unreachable.threshold = fmt.Sprintf("probe failure rate > %.0f%% (last 30s)", e.cfg.ProbeDownPct)
	errs.threshold = fmt.Sprintf("probe failure rate > %.0f%% (last 30s)", e.cfg.ProbeWarnPct)
	latency.threshold = fmt.Sprintf("p95 response > %.0f ms (last 30s)", e.cfg.ProbeP95Ms)

	st, ok := e.probeStatsFor(svc)
	if ok && s.ReadyEndpoints > 0 {
		detail := map[string]any{"probes": st.N, "failed": st.Failed, "errors": st.Errors, "p95Ms": st.P95Ms}
		byNode := map[string]string{}
		for node, c := range st.ByNode {
			byNode[node] = fmt.Sprintf("%d/%d failed", c[1], c[0])
		}
		detail["byNode"] = byNode
		for k, v := range base {
			detail[k] = v
		}
		failText := fmt.Sprintf("%.0f%% of %d probes failed (%s)", st.FailPct, st.N, st.TopError)
		unreachable.detail, errs.detail, latency.detail = detail, detail, detail
		unreachable.observed, errs.observed = failText, failText
		unreachable.value, errs.value = st.FailPct, st.FailPct
		unreachable.violated = st.FailPct > e.cfg.ProbeDownPct
		unreachable.healthy = st.FailPct <= e.cfg.ProbeDownPct*e.cfg.Hysteresis
		// A service that is mostly failing is "unreachable"; the milder rule covers the band below it.
		errs.violated = st.FailPct > e.cfg.ProbeWarnPct && st.FailPct <= e.cfg.ProbeDownPct
		errs.healthy = st.FailPct <= e.cfg.ProbeWarnPct*e.cfg.Hysteresis
		if st.OKs >= minProbeSamples {
			latency.value = st.P95Ms
			latency.observed = fmt.Sprintf("p95 %.0f ms over %d successful probes", st.P95Ms, st.OKs)
			latency.violated, latency.healthy = e.level(st.P95Ms, e.cfg.ProbeP95Ms)
		}
	}
	return []observation{unreachable, errs, latency}
}

// observeLinks judges each configured Pod-to-Service connection from the samples
// taken inside the source Pod. Every configured link always yields an
// observation, neutral while there is too little data, so a quiet agent never
// looks like a vanished link.
func (e *Engine) observeLinks(in Inputs) []observation {
	var out []observation
	for _, l := range in.Links {
		o := observation{
			key: key{RuleLinkFailed, l.SrcPodName + " → " + l.DstService}, source: sourceK8s, targetType: TargetPod, severity: Warning,
			threshold: fmt.Sprintf("probe failure rate > %.0f%% (last 15s, from inside the source Pod)", e.cfg.ProbeDownPct),
		}
		if st, ok := e.linkStatsFor(l.ID); ok {
			o.value = st.FailPct
			o.violated = st.FailPct > e.cfg.ProbeDownPct
			o.healthy = st.FailPct <= e.cfg.ProbeDownPct*e.cfg.Hysteresis
			o.observed = fmt.Sprintf("%s (%d of %d probes failed)", st.TopError, st.Failed, st.N)
			detail := map[string]any{
				"fromPod": l.SrcPodName, "fromService": l.SrcService, "toService": l.DstService,
				"node": st.Node, "error": st.TopError, "failed": st.Failed, "probes": st.N,
			}
			// Evidence for "the destination itself is fine": what probes from the nodes saw.
			if svcStats, ok := e.probeStatsFor(l.DstService); ok {
				detail["destinationFailPctFromNodes"] = svcStats.FailPct
			}
			o.detail = detail
		}
		out = append(out, o)
	}
	return out
}

var unhealthyReasons = map[string]bool{
	"CrashLoopBackOff": true, "OOMKilled": true, "ImagePullBackOff": true, "ErrImagePull": true,
	"CreateContainerConfigError": true, "CreateContainerError": true, "Error": true,
}

func (e *Engine) observePods(in Inputs) []observation {
	nodeCPU := map[string]int64{}
	for _, n := range in.K8s.Nodes {
		nodeCPU[n.Name] = n.CPUMilli
	}
	usage := map[string]systems.PodUsage{}
	for _, n := range in.Nodes {
		if n.Last == nil || n.Status != "up" {
			continue
		}
		for _, u := range n.Last.Pods {
			usage[u.UID] = u
		}
	}

	var out []observation
	for _, p := range in.K8s.Pods {
		if p.Terminating {
			continue
		}
		name := p.FullName()
		bad := unhealthyReasons[p.Reason] || p.Phase == "Failed"
		out = append(out, observation{
			key: key{RulePodUnhealthy, name}, source: sourceK8s, targetType: TargetPod, severity: Warning,
			violated: bad, healthy: !bad && p.Ready, value: float64(p.Restarts),
			threshold: "no CrashLoopBackOff / OOMKilled / image pull failure",
			observed:  strings.TrimSpace(fmt.Sprintf("%s (restarts %d)", p.Reason, p.Restarts)),
			detail:    map[string]any{"node": p.Node, "reason": p.Reason, "restarts": p.Restarts},
		})

		u, ok := usage[p.UID]
		if !ok {
			continue
		}
		if p.CPULimitMilli > 0 && nodeCPU[p.Node] > 0 {
			usedMilli := u.CPUPct / 100 * float64(nodeCPU[p.Node])
			ratio := usedMilli / float64(p.CPULimitMilli) * 100
			violated, healthy := e.level(ratio, e.cfg.PodCPUPct)
			out = append(out, observation{
				key: key{RulePodCPU, name}, source: sourceK8s, targetType: TargetPod, severity: Warning,
				violated: violated, healthy: healthy, value: ratio,
				threshold: fmt.Sprintf("CPU > %.0f%% of limit (%dm)", e.cfg.PodCPUPct, p.CPULimitMilli),
				observed:  fmt.Sprintf("CPU %.1f%% of limit (%.0fm)", ratio, usedMilli),
				detail:    map[string]any{"node": p.Node},
			})
		}
		if p.MemLimitBytes > 0 {
			ratio := float64(u.MemBytes) / float64(p.MemLimitBytes) * 100
			violated, healthy := e.level(ratio, e.cfg.PodMemPct)
			out = append(out, observation{
				key: key{RulePodMemory, name}, source: sourceK8s, targetType: TargetPod, severity: Warning,
				violated: violated, healthy: healthy, value: ratio,
				threshold: fmt.Sprintf("memory > %.0f%% of limit (%d MiB)", e.cfg.PodMemPct, p.MemLimitBytes>>20),
				observed:  fmt.Sprintf("memory %.1f%% of limit (%d MiB)", ratio, u.MemBytes>>20),
				detail:    map[string]any{"node": p.Node},
			})
		}
	}
	return out
}

func (e *Engine) buildView(in Inputs) View {
	v := View{At: in.Now, Recent: append([]Incident(nil), e.recent...)}
	byTarget := map[key]*Incident{}
	for k, st := range e.states {
		if st.incident != nil {
			byTarget[k] = st.incident
			v.Open = append(v.Open, *st.incident)
		}
	}
	sort.Slice(v.Open, func(i, j int) bool {
		if v.Open[i].Severity != v.Open[j].Severity {
			return v.Open[i].Severity == Critical
		}
		return v.Open[i].StartedAt.Before(v.Open[j].StartedAt)
	})

	live := map[string]bool{}
	usage := map[string]systems.PodUsage{}
	for _, n := range in.Nodes {
		if n.Last != nil && n.Status == "up" {
			for _, u := range n.Last.Pods {
				usage[u.UID] = u
			}
		}
	}
	nodeCPU := map[string]int64{}
	if in.K8s != nil {
		for _, n := range in.K8s.Nodes {
			nodeCPU[n.Name] = n.CPUMilli
		}
	}

	if in.K8s != nil {
		for _, s := range in.K8s.Services {
			if len(s.Selector) == 0 {
				continue
			}
			ss := ServiceStatus{Key: s.Key(), Namespace: s.Namespace, Name: s.Name, Ready: s.ReadyEndpoints, Total: s.TotalEndpoints, Pods: len(s.PodUIDs)}
			e.fillUsage(&ss, s, in.K8s.PodByUID(), usage, nodeCPU)
			if st, ok := e.probeStatsFor(s.Key()); ok {
				ss.ProbeN, ss.P95Ms, ss.FailPct, ss.ProbeError = st.N, st.P95Ms, st.FailPct, st.TopError
			}
			if !s.HasSlices {
				ss.Status = Unknown
				v.Services = append(v.Services, ss)
				continue
			}
			ss.Status = Up
			add := func(inc *Incident, status Status) {
				if inc == nil {
					return
				}
				ss.Incidents = append(ss.Incidents, inc.ID)
				if ss.Reason == "" || status == Down {
					ss.Reason = inc.Observed
				}
				if status == Down || ss.Status == Up {
					ss.Status = status
				}
			}
			for k, inc := range byTarget {
				if k.rule == RuleLinkFailed && inc.Detail["fromService"] == s.Key() {
					add(inc, Warn)
					ss.Reason = fmt.Sprintf("%s → %s: %s", inc.Detail["fromPod"], inc.Detail["toService"], inc.Observed)
				}
			}
			add(byTarget[key{RuleServiceDegraded, s.Key()}], Warn)
			add(byTarget[key{RuleProbeErrors, s.Key()}], Warn)
			add(byTarget[key{RuleLatency, s.Key()}], Warn)
			podsByUID := in.K8s.PodByUID()
			for _, uid := range s.PodUIDs {
				p := podsByUID[uid]
				if p == nil {
					continue
				}
				for _, r := range []string{RulePodCPU, RulePodMemory, RulePodUnhealthy} {
					if inc := byTarget[key{r, p.FullName()}]; inc != nil {
						add(inc, Warn)
						if ss.Reason == inc.Observed || ss.Reason == "" {
							ss.Reason = p.FullName() + ": " + inc.Observed
						}
					}
				}
			}
			add(byTarget[key{RuleUnreachable, s.Key()}], Down)
			add(byTarget[key{RuleServiceDown, s.Key()}], Down)
			v.Services = append(v.Services, ss)
		}
		sort.Slice(v.Services, func(i, j int) bool {
			if rank(v.Services[i].Status) != rank(v.Services[j].Status) {
				return rank(v.Services[i].Status) > rank(v.Services[j].Status)
			}
			return v.Services[i].Key < v.Services[j].Key
		})
	}

	names := map[string]*NodeStatus{}
	get := func(name string) *NodeStatus {
		if n := names[name]; n != nil {
			return n
		}
		n := &NodeStatus{Name: name, Status: Unknown}
		names[name] = n
		return n
	}
	for _, n := range in.Nodes {
		if n.Name == "" {
			continue
		}
		ns := get(n.Name)
		ns.Status = Up
		if n.Last != nil {
			ns.CPUPct, ns.MemPct = n.Last.CPUPct, n.Last.MemPct
		}
	}
	if in.K8s != nil {
		for _, n := range in.K8s.Nodes {
			if ns := get(n.Name); ns.Status == Unknown {
				ns.Status = Up
			}
		}
	}
	for name, ns := range names {
		for _, r := range []string{RuleNodeCPU, RuleNodeMemory, RuleNodePressure} {
			if inc := byTarget[key{r, name}]; inc != nil {
				ns.Incidents = append(ns.Incidents, inc.ID)
				ns.Status, ns.Reason = Warn, inc.Observed
			}
		}
		if inc := byTarget[key{RuleNodeDown, name}]; inc != nil {
			ns.Incidents = append(ns.Incidents, inc.ID)
			ns.Status, ns.Reason = Down, inc.Observed
		}
		v.Nodes = append(v.Nodes, *ns)
		if ns.Status != Unknown {
			e.record("node/"+name, Point{T: in.Now.UnixMilli(), C: ns.CPUPct, M: ns.MemPct}, live)
		}
	}
	for _, ss := range v.Services {
		pt := Point{T: in.Now.UnixMilli(), C: ss.CPUMilli, M: float64(ss.MemBytes)}
		if ss.ProbeN > 0 {
			f := ss.FailPct
			pt.F = &f
			if p := ss.P95Ms; p > 0 {
				pt.P = &p
			}
		}
		e.record("service/"+ss.Key, pt, live)
	}
	for k := range e.hist { // forget targets that no longer exist
		if !live[k] {
			delete(e.hist, k)
		}
	}
	sort.Slice(v.Nodes, func(i, j int) bool { return v.Nodes[i].Name < v.Nodes[j].Name })
	return v
}

// fillUsage sums the service's Pods' usage and limits. A limit total is only
// reported when every Pod has one, because a partial sum would overstate usage.
func (e *Engine) fillUsage(ss *ServiceStatus, s k8s.Service, pods map[string]*k8s.Pod, usage map[string]systems.PodUsage, nodeCPU map[string]int64) {
	allCPU, allMem := len(s.PodUIDs) > 0, len(s.PodUIDs) > 0
	for _, uid := range s.PodUIDs {
		p := pods[uid]
		if p == nil {
			continue
		}
		if u, ok := usage[uid]; ok {
			ss.CPUMilli += u.CPUPct / 100 * float64(nodeCPU[p.Node])
			ss.MemBytes += u.MemBytes
		}
		if p.CPULimitMilli > 0 {
			ss.CPULimitMilli += p.CPULimitMilli
		} else {
			allCPU = false
		}
		if p.MemLimitBytes > 0 {
			ss.MemLimitBytes += p.MemLimitBytes
		} else {
			allMem = false
		}
	}
	if !allCPU {
		ss.CPULimitMilli = 0
	}
	if !allMem {
		ss.MemLimitBytes = 0
	}
}

func rank(s Status) int {
	switch s {
	case Down:
		return 3
	case Warn:
		return 2
	case Unknown:
		return 1
	}
	return 0
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
