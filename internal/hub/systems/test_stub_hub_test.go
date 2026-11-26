//go:build testing

package systems

import (
	"testing"

	"argus/internal/common"
	esystem "argus/internal/entities/system"
	"argus/internal/hub/ws"
	"github.com/fxamacker/cbor/v2"
	"github.com/lxzan/gws"

	_ "argus/internal/migrations"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	pbtests "ganigeorgiev.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// idleWsServer is a gws event handler for tests that serve one WebSocket connection by hand.
type idleWsServer struct{ ws.Handler }

func (*idleWsServer) OnClose(*gws.Conn, error) {}

// stubHub implements hubLike using a plain pocketbase test app, so DB tests can
// run in-package without an import cycle to internal/hub (which imports this package).
type stubHub struct{ core.App }

func (stubHub) GetSSHKey(dataDir string) (ssh.Signer, error) { return nil, nil }

// newTestSystemWithHub creates a System backed by a real (temp) database, along
// with a matching "systems" record, for tests that need to exercise DB reads/writes.
func newTestSystemWithHub(t *testing.T) (*System, *pbtests.TestApp) {
	t.Helper()
	testApp, err := pbtests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(testApp.Cleanup)

	sm := &SystemManager{hub: stubHub{testApp}}

	col, err := testApp.FindCachedCollectionByNameOrId("systems")
	require.NoError(t, err)
	systemRecord := core.NewRecord(col)
	systemRecord.Set("name", "test-system")
	systemRecord.Set("host", "127.0.0.1")
	systemRecord.Set("port", "45876")
	require.NoError(t, testApp.SaveNoValidate(systemRecord))

	sys := &System{Id: systemRecord.Id, manager: sm}
	return sys, testApp
}

// sequenceDataClient answers each GetData request with the next queued payload.
type sequenceDataClient struct {
	gws.BuiltinEventHandler
	responses chan esystem.CombinedData
}

func (c *sequenceDataClient) OnMessage(conn *gws.Conn, message *gws.Message) {
	defer message.Close()
	var req common.HubRequest[cbor.RawMessage]
	if err := cbor.Unmarshal(message.Bytes(), &req); err != nil || req.Action != common.GetData {
		return
	}
	data, _ := cbor.Marshal(<-c.responses)
	response, _ := cbor.Marshal(common.AgentResponse{Id: req.Id, Data: data})
	_ = conn.WriteMessage(gws.OpcodeBinary, response)
}
