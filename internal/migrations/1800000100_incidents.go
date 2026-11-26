package migrations

import (
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	m "ganigeorgiev.com/pocketbase/pocketbase/migrations"
)

// incidents stores what the detection engine found: one record per incident,
// from the moment it opened until it recovered.
func init() {
	m.Register(func(app core.App) error {
		if _, err := app.FindCollectionByNameOrId("incidents"); err == nil {
			return nil
		}
		c := core.NewBaseCollection("incidents")
		authed := "@request.auth.id != ''"
		c.ListRule, c.ViewRule = &authed, &authed // read-only for logged-in users; only the hub writes

		c.Fields.Add(
			&core.TextField{Name: "incident_id", Required: true},
			&core.TextField{Name: "rule", Required: true},
			&core.SelectField{Name: "target_type", Required: true, Values: []string{"service", "pod", "node"}, MaxSelect: 1},
			&core.TextField{Name: "target", Required: true},
			&core.SelectField{Name: "severity", Required: true, Values: []string{"WARNING", "CRITICAL"}, MaxSelect: 1},
			&core.BoolField{Name: "open"},
			&core.TextField{Name: "threshold"},
			&core.TextField{Name: "observed"},
			&core.NumberField{Name: "peak"},
			&core.TextField{Name: "window"},
			&core.DateField{Name: "started_at"},
			&core.DateField{Name: "recovered_at"},
			&core.TextField{Name: "note"},
			&core.JSONField{Name: "detail", MaxSize: 1 << 20},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		c.AddIndex("idx_incidents_incident_id", true, "incident_id", "")
		c.AddIndex("idx_incidents_open", false, "open, started_at", "")
		return app.Save(c)
	}, nil)
}
