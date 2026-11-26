package migrations

import (
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	m "ganigeorgiev.com/pocketbase/pocketbase/migrations"
)

// Collections of features this product does not ship (notification alerts,
// SMART, ZFS, systemd services).
var unusedCollections = []string{
	"alerts_history", "alerts", "quiet_hours", "smart_devices", "systemd_services", "zfs_pools",
}

func init() {
	m.Register(func(app core.App) error {
		for _, name := range unusedCollections {
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
