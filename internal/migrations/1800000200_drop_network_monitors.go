package migrations

import (
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	m "ganigeorgiev.com/pocketbase/pocketbase/migrations"
)

// The user-defined network monitor feature is not part of this product. Its history
// collection goes first because it holds a relation to the monitors collection.
var networkMonitorCollections = []string{"network_monitor_stats", "network_monitors"}

func init() {
	m.Register(func(app core.App) error {
		for _, name := range networkMonitorCollections {
			collection, err := app.FindCollectionByNameOrId(name)
			if err != nil {
				continue // already gone
			}
			if err := app.Delete(collection); err != nil {
				return err
			}
		}
		return nil
	}, nil)
}
