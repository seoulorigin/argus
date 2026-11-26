//go:build testing

package records_test

import (
	"fmt"
	"testing"
	"time"

	"argus/internal/records"
	"argus/internal/tests"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteOldRecords tests the main DeleteOldRecords function
func TestDeleteOldRecords(t *testing.T) {
	hub, err := tests.NewTestHub(t.TempDir())
	require.NoError(t, err)
	defer hub.Cleanup()

	rm := records.NewRecordManager(hub)

	// Create test user for alerts history
	user, err := tests.CreateUser(hub, "test@example.com", "testtesttest")
	require.NoError(t, err)

	// Create test system
	system, err := tests.CreateRecord(hub, "systems", map[string]any{
		"name":   "test-system",
		"host":   "localhost",
		"port":   "45876",
		"status": "up",
		"users":  []string{user.Id},
	})
	require.NoError(t, err)

	now := time.Now()

	// Create old system_stats records that should be deleted
	var record *core.Record
	record, err = tests.CreateRecord(hub, "system_stats", map[string]any{
		"system": system.Id,
		"type":   "1m",
		"stats":  `{"cpu": 50.0, "mem": 1024}`,
	})
	require.NoError(t, err)
	// created is autodate field, so we need to set it manually
	record.SetRaw("created", now.UTC().Add(-2*time.Hour).Format(types.DefaultDateLayout))
	err = hub.SaveNoValidate(record)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.InDelta(t, record.GetDateTime("created").Time().UTC().Unix(), now.UTC().Add(-2*time.Hour).Unix(), 1)
	require.Equal(t, record.Get("system"), system.Id)
	require.Equal(t, record.Get("type"), "1m")

	// Create recent system_stats record that should be kept
	_, err = tests.CreateRecord(hub, "system_stats", map[string]any{
		"system":  system.Id,
		"type":    "1m",
		"stats":   `{"cpu": 30.0, "mem": 512}`,
		"created": now.Add(-30 * time.Minute), // 30 minutes old, should be kept
	})
	require.NoError(t, err)

	// Count records before deletion
	systemStatsCountBefore, err := hub.CountRecords("system_stats")
	require.NoError(t, err)

	// Run deletion
	rm.DeleteOldRecords()

	// Count records after deletion
	systemStatsCountAfter, err := hub.CountRecords("system_stats")
	require.NoError(t, err)

	// Verify old system stats were deleted
	assert.Less(t, systemStatsCountAfter, systemStatsCountBefore, "Old system stats should be deleted")

}

// TestDeleteOldSystemStats tests the deleteOldSystemStats function
func TestDeleteOldSystemStats(t *testing.T) {
	hub, err := tests.NewTestHub(t.TempDir())
	require.NoError(t, err)
	defer hub.Cleanup()

	// Create test system
	user, err := tests.CreateUser(hub, "test@example.com", "testtesttest")
	require.NoError(t, err)

	system, err := tests.CreateRecord(hub, "systems", map[string]any{
		"name":   "test-system",
		"host":   "localhost",
		"port":   "45876",
		"status": "up",
		"users":  []string{user.Id},
	})
	require.NoError(t, err)

	now := time.Now().UTC()

	// Test data for different record types and their retention periods
	testCases := []struct {
		recordType   string
		retention    time.Duration
		shouldBeKept bool
		ageFromNow   time.Duration
		description  string
	}{
		{"1m", time.Hour, true, 30 * time.Minute, "1m record within 1 hour should be kept"},
		{"1m", time.Hour, false, 2 * time.Hour, "1m record older than 1 hour should be deleted"},
		{"10m", 12 * time.Hour, true, 6 * time.Hour, "10m record within 12 hours should be kept"},
		{"10m", 12 * time.Hour, false, 24 * time.Hour, "10m record older than 12 hours should be deleted"},
		{"20m", 24 * time.Hour, true, 12 * time.Hour, "20m record within 24 hours should be kept"},
		{"20m", 24 * time.Hour, false, 48 * time.Hour, "20m record older than 24 hours should be deleted"},
		{"120m", 7 * 24 * time.Hour, true, 3 * 24 * time.Hour, "120m record within 7 days should be kept"},
		{"120m", 7 * 24 * time.Hour, false, 10 * 24 * time.Hour, "120m record older than 7 days should be deleted"},
		{"480m", 30 * 24 * time.Hour, true, 15 * 24 * time.Hour, "480m record within 30 days should be kept"},
		{"480m", 30 * 24 * time.Hour, false, 45 * 24 * time.Hour, "480m record older than 30 days should be deleted"},
	}

	// Create test records for both system_stats and container_stats
	collections := []string{"system_stats", "container_stats"}
	recordIds := make(map[string][]string)

	for _, collection := range collections {
		recordIds[collection] = make([]string, 0)

		for i, tc := range testCases {
			recordTime := now.Add(-tc.ageFromNow)

			var stats string
			if collection == "system_stats" {
				stats = fmt.Sprintf(`{"cpu": %d.0, "mem": %d}`, i*10, i*100)
			} else {
				stats = fmt.Sprintf(`[{"name": "container%d", "cpu": %d.0, "mem": %d}]`, i, i*5, i*50)
			}

			record, err := tests.CreateRecord(hub, collection, map[string]any{
				"system": system.Id,
				"type":   tc.recordType,
				"stats":  stats,
			})
			require.NoError(t, err)
			record.SetRaw("created", recordTime.Format(types.DefaultDateLayout))
			err = hub.SaveNoValidate(record)
			require.NoError(t, err)
			recordIds[collection] = append(recordIds[collection], record.Id)
		}
	}

	// Run deletion
	err = records.DeleteOldSystemStats(hub)
	require.NoError(t, err)

	// Verify results
	for _, collection := range collections {
		for i, tc := range testCases {
			recordId := recordIds[collection][i]

			// Try to find the record
			_, err := hub.FindRecordById(collection, recordId)

			if tc.shouldBeKept {
				assert.NoError(t, err, "Record should exist: %s", tc.description)
			} else {
				assert.Error(t, err, "Record should be deleted: %s", tc.description)
			}
		}
	}
}
