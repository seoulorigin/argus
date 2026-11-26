//go:build testing

package systems

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"argus/internal/common"
	"argus/internal/entities/container"
	"argus/internal/entities/system"
	"argus/internal/hub/ws"
	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/blang/semver"
	"github.com/fxamacker/cbor/v2"
	"github.com/lxzan/gws"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeNilEntries(t *testing.T) {
	data := &system.CombinedData{
		Containers: []*container.Stats{nil, {Id: "abc"}, nil},
		Stats:      system.Stats{},
	}
	sanitizeNilEntries(data)
	assert.Len(t, data.Containers, 1)
	assert.Equal(t, "abc", data.Containers[0].Id)

	// only nils: container slice becomes empty, so Containers[0] is never read
	data = &system.CombinedData{Containers: []*container.Stats{nil}}
	sanitizeNilEntries(data)
	assert.Empty(t, data.Containers)

	assert.NotPanics(t, func() { sanitizeNilEntries(nil) })
}

// fixedDataClient answers every GetData request with the same payload.
type fixedDataClient struct {
	gws.BuiltinEventHandler
	data *system.CombinedData
}

func (c *fixedDataClient) OnMessage(conn *gws.Conn, message *gws.Message) {
	defer message.Close()
	var req common.HubRequest[cbor.RawMessage]
	if err := cbor.Unmarshal(message.Bytes(), &req); err != nil || req.Action != common.GetData {
		return
	}
	// nil pointers in the payload encode as CBOR nulls, as a buggy agent would send
	response, _ := cbor.Marshal(common.AgentResponse{Id: req.Id, SystemData: c.data})
	_ = conn.WriteMessage(gws.OpcodeBinary, response)
}

// A full poll with null containers and filesystems used to panic in
// createRecords (#2831). The nulls must be gone before sys.data is published.
func TestUpdateDropsNullEntriesFromAgent(t *testing.T) {
	sys, app := newTestSystemWithHub(t)

	agentData := &system.CombinedData{
		Containers: []*container.Stats{nil, {Id: "abc", Name: "web"}},
		Stats:      system.Stats{},
	}

	connections := make(chan *ws.WsConn, 1)
	upgrader := gws.NewUpgrader(&idleWsServer{}, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		wsConn := ws.NewWsConnection(conn, semver.MustParse("0.20.0"))
		conn.Session().Store("wsConn", wsConn)
		connections <- wsConn
		conn.ReadLoop()
	}))
	t.Cleanup(server.Close)

	conn, _, err := gws.NewClient(&fixedDataClient{data: agentData}, &gws.ClientOption{Addr: "ws" + strings.TrimPrefix(server.URL, "http")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.NetConn().Close() })
	go conn.ReadLoop()

	select {
	case sys.WsConn = <-connections:
	case <-time.After(3 * time.Second):
		t.Fatal("websocket connection was not established")
	}

	require.NotPanics(t, func() { require.NoError(t, sys.update()) })

	require.Len(t, sys.data.Containers, 1)
	assert.Equal(t, "abc", sys.data.Containers[0].Id)

	count, err := app.CountRecords("container_stats", dbx.HashExp{"system": sys.Id})
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)
}
