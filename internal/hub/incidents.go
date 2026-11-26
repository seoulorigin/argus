package hub

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"argus/internal/hub/detect"
	"argus/internal/hub/k8s"
	"argus/internal/hub/utils"

	"ganigeorgiev.com/pocketbase/dbx"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/types"
)

// incidentRetention is how long recovered incidents stay in the database.
const incidentRetention = 7 * 24 * time.Hour

// startDetection runs the detection engine every 5 seconds and stores its incidents.
func (h *Hub) startDetection() error {
	h.engine = detect.NewEngine(detect.DefaultConfig())

	// The engine keeps its state in memory, so incidents left open by a previous
	// run cannot be continued. Close them; anything still wrong opens again within 15s.
	if err := h.closeStaleIncidents(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.App.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		cancel()
		return e.Next()
	})
	go h.runDetection(ctx)
	return nil
}

func (h *Hub) runDetection(ctx context.Context) {
	t := time.NewTicker(k8s.Interval)
	defer t.Stop()
	lastCleanup := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var snap *k8s.Snapshot
		var links []k8s.LinkTarget
		if h.k8s != nil {
			snap = h.k8s.Snapshot()
			if snap != nil {
				links = snap.LinkTargets(h.linkPairs)
			}
		}
		now := time.Now()
		events := h.engine.Evaluate(detect.Inputs{Now: now, K8s: snap, Nodes: h.sm.Observations(), Links: links})
		for _, ev := range events {
			if err := h.saveIncident(ev); err != nil {
				slog.Error("Saving incident failed", "incident", ev.Incident.ID, "err", err)
				continue
			}
			if ev.Kind != detect.Updated {
				slog.Info("Incident "+string(ev.Kind), "rule", ev.Incident.Rule, "target", ev.Incident.Target,
					"severity", ev.Incident.Severity, "observed", ev.Incident.Observed)
			}
		}
		if now.Sub(lastCleanup) > time.Hour {
			lastCleanup = now
			cutoff := now.Add(-incidentRetention).UTC().Format(types.DefaultDateLayout)
			if _, err := h.DB().NewQuery("DELETE FROM incidents WHERE open = FALSE AND recovered_at < {:cutoff}").
				Bind(dbx.Params{"cutoff": cutoff}).Execute(); err != nil {
				slog.Warn("Incident cleanup failed", "err", err)
			}
		}
	}
}

func (h *Hub) closeStaleIncidents() error {
	_, err := h.DB().NewQuery("UPDATE incidents SET open = FALSE, recovered_at = {:now}, note = 'hub restarted' WHERE open = TRUE").
		Bind(dbx.Params{"now": time.Now().UTC().Format(types.DefaultDateLayout)}).Execute()
	return err
}

// saveIncident writes an opened, updated or recovered incident.
func (h *Hub) saveIncident(ev detect.Event) error {
	inc := ev.Incident
	rec, err := h.FindFirstRecordByFilter("incidents", "incident_id = {:id}", dbx.Params{"id": inc.ID})
	if err != nil {
		col, cerr := h.FindCachedCollectionByNameOrId("incidents")
		if cerr != nil {
			return cerr
		}
		rec = core.NewRecord(col)
		rec.Set("incident_id", inc.ID)
		rec.Set("rule", inc.Rule)
		rec.Set("target_type", inc.TargetType)
		rec.Set("target", inc.Target)
		rec.Set("started_at", inc.StartedAt.UTC())
		rec.Set("threshold", inc.Threshold)
		rec.Set("window", inc.Window)
		rec.Set("detail", inc.Detail)
	}
	rec.Set("severity", string(inc.Severity))
	rec.Set("open", inc.Open)
	rec.Set("observed", inc.Observed)
	rec.Set("peak", inc.Peak)
	rec.Set("note", inc.Note)
	if inc.RecoveredAt != nil {
		rec.Set("recovered_at", inc.RecoveredAt.UTC())
	}
	return h.SaveNoValidate(rec)
}

// getStatus returns the engine's current service and node states with open and recent incidents.
func (h *Hub) getStatus(e *core.RequestEvent) error {
	e.Response.Header().Set("Cache-Control", "no-store")
	view := h.engine.View()
	cluster, _ := utils.GetEnv("CLUSTER_NAME")
	return e.JSON(http.StatusOK, struct {
		detect.View
		Cluster string `json:"cluster"`
	}{view, cluster})
}

// getHistory returns the last 15 minutes of samples for one service or node.
// key is "service/<namespace>/<name>" or "node/<name>".
func (h *Hub) getHistory(e *core.RequestEvent) error {
	e.Response.Header().Set("Cache-Control", "no-store")
	return e.JSON(http.StatusOK, h.engine.History(e.Request.URL.Query().Get("key")))
}
