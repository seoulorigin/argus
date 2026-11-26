//go:build testing

package systems

import (
	"context"
	"net"
	"testing"
	"time"

	"argus/internal/common"
	"argus/internal/entities/system"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

func TestSetDownAfterContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// manager is nil on purpose: setDown must bail out before touching the app
	sys := &System{Status: up, ctx: ctx}

	if err := sys.setDown(nil); err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if sys.Status != up {
		t.Fatalf("status should be untouched, got %q", sys.Status)
	}
}

func TestSSHDisabledSkipsSSHFallback(t *testing.T) {
	t.Setenv("DISABLE_SSH", "true")
	// manager is nil on purpose: any SSH attempt would panic
	sys := &System{}

	var result string
	err := sys.request(context.Background(), common.SyncNetworkMonitors, nil, &result)
	require.ErrorIs(t, err, errSSHDisabled)

	_, err = sys.fetchDataFromAgent(common.DataRequestOptions{})
	require.ErrorIs(t, err, errSSHDisabled)
}

func TestSSHFallbackDialsAgentUnlessDisabled(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hubEnv   string
		wantDial bool
	}{
		{name: "enabled", wantDial: true},
		{name: "disabled on hub", hubEnv: "DISABLE_SSH"},
		{name: "disabled on hub with prefix", hubEnv: "ARGUS_HUB_DISABLE_SSH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.hubEnv != "" {
				t.Setenv(tc.hubEnv, "true")
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			accepted := make(chan struct{}, 1)
			go func() {
				if conn, err := listener.Accept(); err == nil {
					accepted <- struct{}{}
					conn.Close()
				}
			}()

			host, port, _ := net.SplitHostPort(listener.Addr().String())
			sm := &SystemManager{sshConfig: &ssh.ClientConfig{HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second}}
			sys := &System{Host: host, Port: port, Status: down, manager: sm, ctx: context.Background()}

			_, err = sys.fetchDataFromAgent(common.DataRequestOptions{})
			require.Error(t, err) // listener isn't a real agent
			if !tc.wantDial {
				require.ErrorIs(t, err, errSSHDisabled)
			}

			select {
			case <-accepted:
				require.True(t, tc.wantDial, "hub must not dial SSH when it is disabled")
			case <-time.After(200 * time.Millisecond):
				require.False(t, tc.wantDial, "hub must dial SSH when it is enabled")
			}
		})
	}
}

func TestCreateRecordsSavesCpuSockets(t *testing.T) {
	sys, app := newTestSystemWithHub(t)
	_, err := sys.createRecords(&system.CombinedData{Details: &system.Details{Hostname: "host", Sockets: 2}})
	require.NoError(t, err)
	record, err := app.FindRecordById("system_details", sys.Id)
	require.NoError(t, err)
	require.Equal(t, 2, record.GetInt("sockets"))
}
