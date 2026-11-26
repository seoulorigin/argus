//go:build testing

package systems

import (
	"testing"

	"argus/internal/entities/container"
	"ganigeorgiev.com/pocketbase/dbx"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Record IDs are derived from agent-supplied values, so one system must never be
// able to take over or overwrite a record that belongs to another system.

func newOtherSystem(t *testing.T, app core.App) string {
	t.Helper()
	col, err := app.FindCachedCollectionByNameOrId("systems")
	require.NoError(t, err)
	rec := core.NewRecord(col)
	rec.Set("name", "other-system")
	rec.Set("host", "127.0.0.2")
	rec.Set("port", "45876")
	require.NoError(t, app.SaveNoValidate(rec))
	return rec.Id
}

func TestCreateContainerRecordsDoesNotTakeOverOtherSystemsRecord(t *testing.T) {
	sys, app := newTestSystemWithHub(t)
	other := newOtherSystem(t, app)

	require.NoError(t, createContainerRecords(app, []*container.Stats{{Id: "shared-id", Name: "victim"}}, other))
	require.NoError(t, createContainerRecords(app, []*container.Stats{{Id: "shared-id", Name: "attacker"}}, sys.Id))

	var rec struct {
		System string `db:"system"`
		Name   string `db:"name"`
	}
	require.NoError(t, app.DB().Select("system", "name").From("containers").
		Where(dbx.HashExp{"id": "shared-id"}).One(&rec))
	assert.Equal(t, other, rec.System)
	assert.Equal(t, "victim", rec.Name)
}
