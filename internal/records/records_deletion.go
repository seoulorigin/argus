package records

import (
	"fmt"
	"log/slog"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
)

// Delete old records
func (rm *RecordManager) DeleteOldRecords() {
	// Pocketbase cron does not handle errors, log them here.
	rm.app.RunInTransaction(func(txApp core.App) error {
		err := deleteOldSystemStats(txApp)
		if err != nil {
			slog.Error("Error deleting old system stats", "err", err)
		}
		err = deleteOldContainerRecords(txApp)
		if err != nil {
			slog.Error("Error deleting old container records", "err", err)
		}
		return nil
	})
}

// Deletes system_stats records older than what is displayed in the UI
func deleteOldSystemStats(app core.App) error {
	// Collections to process
	collections := [2]string{"system_stats", "container_stats"}

	// Record types and their retention periods
	type RecordDeletionData struct {
		recordType string
		retention  time.Duration
	}
	recordData := []RecordDeletionData{
		{recordType: "1m", retention: time.Hour},             // 1 hour
		{recordType: "10m", retention: 12 * time.Hour},       // 12 hours
		{recordType: "20m", retention: 24 * time.Hour},       // 1 day
		{recordType: "120m", retention: 7 * 24 * time.Hour},  // 7 days
		{recordType: "480m", retention: 30 * 24 * time.Hour}, // 30 days
	}

	now := time.Now().UTC()
	db := app.DB()

	for _, collection := range collections {
		query := db.Delete(collection, dbx.NewExp("type={:type} AND created<{:created}"))
		for _, rd := range recordData {
			if _, err := query.Bind(dbx.Params{
				"type":    rd.recordType,
				"created": getCreatedTimeField(collection, now.Add(-rd.retention)),
			}).Execute(); err != nil {
				return fmt.Errorf("failed to delete from %s: %v", collection, err)
			}
		}
	}
	return nil
}

// Deletes container records that haven't been updated in the last 10 minutes
func deleteOldContainerRecords(app core.App) error {
	now := time.Now().UTC()
	tenMinutesAgo := now.Add(-10 * time.Minute)

	// Delete container records where updated < tenMinutesAgo
	_, err := app.DB().NewQuery("DELETE FROM containers WHERE updated < {:updated}").Bind(dbx.Params{"updated": tenMinutesAgo.UnixMilli()}).Execute()
	if err != nil {
		return fmt.Errorf("failed to delete old container records: %v", err)
	}

	return nil
}
