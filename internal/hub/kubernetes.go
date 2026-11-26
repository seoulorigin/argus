package hub

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"argus/internal/entities/probe"
	"argus/internal/hub/k8s"
	"argus/internal/hub/utils"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
)

// startKubernetes starts reading the cluster if a Kubernetes API is configured.
// Outside a cluster it does nothing and the hub works as a plain node monitor.
func (h *Hub) startKubernetes() error {
	client, err := k8s.NewInClusterClient()
	if err != nil {
		return err
	}
	if client == nil {
		slog.Info("Kubernetes API not configured; cluster view disabled")
		return nil
	}
	h.k8s = k8s.NewSyncer(client)
	h.sm.PodName = h.k8s.PodName
	h.sm.PodHostNetwork = h.k8s.HostNetwork
	domain, _ := utils.GetEnv("CLUSTER_DOMAIN")
	spec, _ := utils.GetEnv("POD_PROBES")
	pairs, err := k8s.ParseLinkPairs(spec)
	if err != nil {
		return fmt.Errorf("POD_PROBES: %w", err)
	}
	h.linkPairs = pairs
	if len(pairs) > 0 {
		slog.Info("Pod-to-Service checks configured", "pairs", len(pairs))
	}
	h.sm.ProbeTargets = func() ([]probe.Target, bool) {
		snap := h.k8s.Snapshot()
		if snap == nil {
			return nil, false
		}
		var out []probe.Target
		for _, t := range snap.ProbeTargets(domain) {
			out = append(out, t.Target)
		}
		for _, l := range snap.LinkTargets(h.linkPairs) {
			out = append(out, l.Target)
		}
		return out, true
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.App.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		cancel()
		return e.Next()
	})
	go h.k8s.Run(ctx)
	slog.Info("Kubernetes cluster view enabled", "interval", k8s.Interval)
	return nil
}

// getKubernetes returns the latest cluster snapshot and its freshness.
func (h *Hub) getKubernetes(e *core.RequestEvent) error {
	if h.k8s == nil {
		return e.JSON(http.StatusOK, map[string]any{"enabled": false})
	}
	return e.JSON(http.StatusOK, map[string]any{
		"enabled":  true,
		"status":   h.k8s.Status(),
		"snapshot": h.k8s.Snapshot(),
	})
}
