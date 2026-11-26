package systems

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"argus/internal/common"
	"argus/internal/hub/transport"
	"argus/internal/hub/utils"
	"argus/internal/hub/ws"

	"argus/internal/entities/container"
	"argus/internal/entities/probe"
	"argus/internal/entities/system"

	"argus"

	"ganigeorgiev.com/pocketbase/dbx"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"github.com/blang/semver"
	"github.com/fxamacker/cbor/v2"
	"github.com/lxzan/gws"
	"golang.org/x/crypto/ssh"
)

type System struct {
	Id             string                         `db:"id"`
	Host           string                         `db:"host"`
	Port           string                         `db:"port"`
	Status         string                         `db:"status"` // Use GetStatus/swapStatus after publishing the system.
	statusMu       sync.RWMutex                   // Protects Status and exchanges used by alert transitions.
	manager        *SystemManager                 // Manager that this system belongs to
	sshMu          sync.Mutex                     // Protects sshTransport creation
	sshTransport   *transport.SSHTransport        // Owns the SSH connection to the agent
	data           *system.CombinedData           // system data from agent
	ctx            context.Context                // Context for stopping the updater
	cancel         context.CancelFunc             // Stops and removes system from updater
	WsConn         *ws.WsConn                     // Handler for agent WebSocket connection
	agentVersion   atomic.Pointer[semver.Version] // Use getAgentVersion/setAgentVersion
	updateTicker   *time.Ticker                   // Ticker for updating the system
	Name           string                         // Node name (set from the systems record)
	probeMu        sync.Mutex                     // Guards probeSamples
	probeSamples   []probe.Sample                 // Samples not yet read by the detection engine
	observed       atomic.Pointer[Observation]    // Latest successful poll, for the detection engine
	failedPolls    atomic.Int32                   // Consecutive failed polls
	detailsFetched atomic.Bool                    // True if static system details have been fetched and saved

	// Serialize persistence from scheduled updates and resumes through commit.
	recordsMu sync.Mutex
}

// errSSHDisabled is returned instead of dialing SSH when DISABLE_SSH is set on the hub.
var errSSHDisabled = errors.New("no WebSocket connection and SSH is disabled")

// sshFallbackDisabled reports whether DISABLE_SSH is set on the hub.
func sshFallbackDisabled() bool {
	disableSSH, _ := utils.GetEnv("DISABLE_SSH")
	return disableSSH == "true"
}

// GetStatus returns the current monitoring status.
func (sys *System) GetStatus() string {
	sys.statusMu.RLock()
	defer sys.statusMu.RUnlock()
	return sys.Status
}

// swapStatus updates the status and returns the previous value as one operation.
func (sys *System) swapStatus(status string) string {
	sys.statusMu.Lock()
	defer sys.statusMu.Unlock()
	previous := sys.Status
	sys.Status = status
	return previous
}

func (sm *SystemManager) NewSystem(systemId string) *System {
	system := &System{
		Id:   systemId,
		data: &system.CombinedData{},
	}
	system.ctx, system.cancel = system.getContext(sm.ctx)
	return system
}

// StartUpdater starts the system updater.
// It first fetches the data from the agent then updates the records.
// If the data is not found or the system is down, it sets the system down.
func (sys *System) StartUpdater() {
	// Channel that can be used to set the system down. Currently only used to
	// allow a short delay for reconnection after websocket connection is closed.
	var downChan chan struct{}

	// Add random jitter to first WebSocket connection to prevent
	// clustering if all agents are started at the same time.
	// SSH connections during hub startup are already staggered.
	var jitter <-chan time.Time
	if sys.WsConn != nil {
		jitter = getJitter()
		// use the websocket connection's down channel to set the system down
		downChan = sys.WsConn.DownChan
	} else {
		// if the system does not have a websocket connection, wait before updating
		// to allow the agent to connect via websocket (makes sure fingerprint is set).
		if !waitForContext(sys.ctx, 11*time.Second) {
			return
		}

	}

	// update immediately if system is not paused (only for ws connections)
	// we'll wait a minute before connecting via SSH to prioritize ws connections
	if sys.GetStatus() != paused && sys.ctx.Err() == nil {
		sys.poll()
	}

	sys.updateTicker = time.NewTicker(time.Duration(interval) * time.Millisecond)
	// Go 1.23+ will automatically stop the ticker when the system is garbage collected, however we seem to need this or testing/synctest will block even if calling runtime.GC()
	defer sys.updateTicker.Stop()

	for {
		select {
		case <-sys.ctx.Done():
			return
		case <-sys.updateTicker.C:
			sys.poll()
		case <-downChan:
			sys.WsConn = nil
			downChan = nil
			_ = sys.setDown(nil)
		case <-jitter:
			sys.updateTicker.Reset(time.Duration(interval) * time.Millisecond)
			sys.poll()
		}
	}
}

// nameContainers replaces the Pod UIDs an agent reports with "namespace/name"
// where the hub's Kubernetes view knows the Pod. Unknown UIDs keep the UID.
func (sys *System) nameContainers(data *system.CombinedData) {
	if sys.manager == nil || sys.manager.PodName == nil {
		return
	}
	for _, c := range data.Containers {
		if name, ok := sys.manager.PodName(c.Id); ok {
			c.Name = name
		}
		if sys.manager.PodHostNetwork != nil && sys.manager.PodHostNetwork(c.Id) {
			c.Bandwidth = [2]uint64{}
		}
	}
}

// poll runs one update and tracks consecutive failures. A node is only declared
// down after downAfterFailures failed polls in a row, so a single dropped
// response does not flip the status.
func (sys *System) poll() {
	if err := sys.update(); err != nil {
		if sys.failedPolls.Add(1) >= downAfterFailures {
			_ = sys.setDown(err)
		}
		return
	}
	sys.failedPolls.Store(0)
}

// update updates the system data and records.
func (sys *System) update() error {
	if sys.GetStatus() == paused {
		sys.handlePaused()
		return nil
	}
	options := common.DataRequestOptions{
		CacheTimeMs: uint16(interval),
	}
	// Hand the agent the current probe targets; it replaces its own list with this one.
	if sys.manager != nil && sys.manager.ProbeTargets != nil {
		targets := sys.manager.ProbeTargets
		if list, known := targets(); known {
			options.ProbesSet = true
			options.Probes = list
		}
	}
	// fetch system details if not already fetched
	if !sys.detailsFetched.Load() {
		options.IncludeDetails = true
	}

	data, err := sys.fetchDataFromAgent(options)
	if err != nil {
		// Ping so a slow agent's connection survives until the next poll. Ping()
		// extends the read deadline without needing a reply (see ws.deadline).
		// Once the agent has been silent for maxAgentSilence, stop pinging and let
		// the deadline close what is likely a dead connection.
		if sys.WsConn != nil && sys.WsConn.IsConnected() && sys.WsConn.TimeSinceLastMessage() < maxAgentSilence {
			_ = sys.WsConn.Ping()
		}
		return err
	}

	// Drop null entries
	sanitizeNilEntries(data)
	sys.nameContainers(data)
	sys.data = data
	sys.storeObservation(data)
	sys.addProbeSamples(data.Probes)

	// create system records
	_, err = sys.createRecords(data)

	// if details were included and fetched successfully, mark details as fetched
	if err == nil && data.Details != nil {
		sys.detailsFetched.Store(true)
	}

	return err
}

func (sys *System) handlePaused() {
	if sys.WsConn == nil {
		// if the system is paused and there's no websocket connection, remove the system
		_ = sys.manager.RemoveSystem(sys.Id)
	} else {
		// Send a ping to the agent to keep the connection alive if the system is paused
		if err := sys.WsConn.Ping(); err != nil {
			sys.manager.hub.Logger().Warn("Failed to ping agent", "system", sys.Id, "err", err)
			_ = sys.manager.RemoveSystem(sys.Id)
		}
	}
}

// createRecords updates the system record and adds system_stats and container_stats records
func (sys *System) createRecords(data *system.CombinedData) (*core.Record, error) {
	sys.recordsMu.Lock()
	defer sys.recordsMu.Unlock()

	systemRecord, err := sys.getRecord(sys.manager.hub)
	if err != nil {
		return nil, err
	}
	hub := sys.manager.hub
	err = hub.RunInTransaction(func(txApp core.App) error {
		// add system_stats record
		systemStatsCollection, err := txApp.FindCachedCollectionByNameOrId("system_stats")
		if err != nil {
			return err
		}
		systemStatsRecord := core.NewRecord(systemStatsCollection)
		systemStatsRecord.Set("system", systemRecord.Id)
		systemStatsRecord.Set("stats", data.Stats)
		systemStatsRecord.Set("type", "1m")
		if err := txApp.SaveNoValidate(systemStatsRecord); err != nil {
			return err
		}

		// add containers and container_stats records
		if len(data.Containers) > 0 {
			if data.Containers[0].Id != "" {
				if err := createContainerRecords(txApp, data.Containers, sys.Id); err != nil {
					return err
				}
			}
			containerStatsCollection, err := txApp.FindCachedCollectionByNameOrId("container_stats")
			if err != nil {
				return err
			}
			containerStatsRecord := core.NewRecord(containerStatsCollection)
			containerStatsRecord.Set("system", systemRecord.Id)
			containerStatsRecord.Set("stats", data.Containers)
			containerStatsRecord.Set("type", "1m")
			if err := txApp.SaveNoValidate(containerStatsRecord); err != nil {
				return err
			}
		}

		// add system details record
		if data.Details != nil {
			if err := createSystemDetailsRecord(txApp, data.Details, sys.Id); err != nil {
				return err
			}
			// sync display name with hostname if enabled (details are fetched once per agent connection)
			if syncNames, _ := utils.GetEnv("SYNC_SYSTEM_NAMES"); syncNames == "true" && data.Details.Hostname != "" {
				systemRecord.Set("name", data.Details.Hostname)
			}
		}

		// update system record (do this last because it triggers alerts and we need above records to be inserted first)
		systemRecord.Set("status", up)
		systemRecord.Set("info", data.Info)
		if err := txApp.SaveNoValidate(systemRecord); err != nil {
			return err
		}
		return nil
	})

	return systemRecord, err
}

// sanitizeNilEntries removes null elements from agent-supplied slices and maps
// so later code can dereference them safely. Agent payloads can contain nulls.
func sanitizeNilEntries(data *system.CombinedData) {
	if data == nil {
		return
	}
	data.Containers = slices.DeleteFunc(data.Containers, func(c *container.Stats) bool { return c == nil })
}

func createSystemDetailsRecord(app core.App, data *system.Details, systemId string) error {
	collectionName := "system_details"
	params := dbx.Params{
		"id":       systemId,
		"system":   systemId,
		"hostname": data.Hostname,
		"kernel":   data.Kernel,
		"cores":    data.Cores,
		"threads":  data.Threads,
		"sockets":  data.Sockets,
		"cpu":      data.CpuModel,
		"os":       data.Os,
		"os_name":  data.OsName,
		"arch":     data.Arch,
		"memory":   data.MemoryTotal,
		"podman":   false,
		"updated":  time.Now().UTC(),
	}
	result, err := app.DB().Update(collectionName, params, dbx.HashExp{"id": systemId}).Execute()
	rowsAffected, _ := result.RowsAffected()
	if err != nil || rowsAffected == 0 {
		_, err = app.DB().Insert(collectionName, params).Execute()
	}
	return err
}

// createContainerRecords creates container records. Record IDs are agent-supplied,
// so an ID owned by another system is left untouched rather than taken over.
func createContainerRecords(app core.App, data []*container.Stats, systemId string) error {
	if len(data) == 0 {
		return nil
	}
	// shared params for all records
	params := dbx.Params{
		"system":  systemId,
		"updated": time.Now().UTC().UnixMilli(),
	}
	valueStrings := make([]string, 0, len(data))
	for i, container := range data {
		suffix := fmt.Sprintf("%d", i)
		valueStrings = append(valueStrings, fmt.Sprintf("({:id%[1]s}, {:system}, {:name%[1]s}, {:image%[1]s}, {:ports%[1]s}, {:status%[1]s}, {:health%[1]s}, {:cpu%[1]s}, {:memory%[1]s}, {:net%[1]s}, {:updateAvailable%[1]s}, {:updated})", suffix))
		params["id"+suffix] = container.Id
		params["name"+suffix] = container.Name
		params["image"+suffix] = container.Image
		params["ports"+suffix] = container.Ports
		params["status"+suffix] = container.Status
		params["health"+suffix] = container.Health
		params["cpu"+suffix] = container.Cpu
		params["memory"+suffix] = container.Mem
		netBytes := container.Bandwidth[0] + container.Bandwidth[1]
		if netBytes == 0 {
			netBytes = uint64((container.NetworkSent + container.NetworkRecv) * 1024 * 1024)
		}
		params["net"+suffix] = netBytes
		params["updateAvailable"+suffix] = container.UpdateAvailable
	}
	queryString := fmt.Sprintf(
		"INSERT INTO containers (id, system, name, image, ports, status, health, cpu, memory, net, updatable, updated) VALUES %s ON CONFLICT(id) DO UPDATE SET name = excluded.name, image = excluded.image, ports = excluded.ports, status = excluded.status, health = excluded.health, cpu = excluded.cpu, memory = excluded.memory, net = excluded.net, updatable = excluded.updatable, updated = excluded.updated WHERE containers.system = excluded.system",
		strings.Join(valueStrings, ","),
	)
	_, err := app.DB().NewQuery(queryString).Bind(params).Execute()
	return err
}

// getRecord retrieves the system record from the database.
// If the record is not found, it removes the system from the manager.
// Other errors, like a query timing out while the hub is stalled, leave the
// system in place so the updater retries on the next tick (issue #2544).
func (sys *System) getRecord(app core.App) (*core.Record, error) {
	record, err := app.FindRecordById("systems", sys.Id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err != nil || record == nil {
		_ = sys.manager.RemoveSystem(sys.Id)
		if err == nil {
			err = fmt.Errorf("system record %s not found", sys.Id)
		}
		return nil, err
	}
	return record, nil
}

// HasUser checks if the given user is in the system's users list.
// Returns true if SHARE_ALL_SYSTEMS is enabled (any authenticated user can access any system).
func (sys *System) HasUser(app core.App, user *core.Record) bool {
	if user == nil {
		return false
	}
	if v, _ := utils.GetEnv("SHARE_ALL_SYSTEMS"); v == "true" {
		return true
	}
	var recordData = struct {
		Users string
	}{}
	err := app.DB().NewQuery("SELECT users FROM systems WHERE id={:id}").
		Bind(dbx.Params{"id": sys.Id}).
		One(&recordData)
	if err != nil || recordData.Users == "" {
		return false
	}
	return strings.Contains(recordData.Users, user.Id)
}

// setDown marks a system as down in the database.
// It takes the original error that caused the system to go down and returns any error
// encountered during the process of updating the system status.
// It is a no-op if the system's context has been cancelled.
func (sys *System) setDown(originalError error) error {
	if status := sys.GetStatus(); status == down || status == paused {
		return nil
	}
	// the updater can race shutdown, and the app may already be disposed by the
	// time we get here, so don't touch the database once the context is cancelled
	if sys.ctx != nil && sys.ctx.Err() != nil {
		return sys.ctx.Err()
	}
	record, err := sys.getRecord(sys.manager.hub)
	if err != nil {
		return err
	}
	if originalError != nil {
		sys.manager.hub.Logger().Error("System down", "system", record.GetString("name"), "err", originalError)
	}
	sys.detailsFetched.Store(false)
	record.Set("status", down)
	return sys.manager.hub.SaveNoValidate(record)
}

func (sys *System) getContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if sys.ctx == nil {
		sys.ctx, sys.cancel = context.WithCancel(ctx)
	}
	return sys.ctx, sys.cancel
}

// request sends a request to the agent, trying WebSocket first, then SSH.
// This is the unified request method that uses the transport abstraction.
func (sys *System) request(ctx context.Context, action common.WebSocketAction, req any, dest any) error {
	// Try WebSocket first
	if sys.WsConn != nil && sys.WsConn.IsConnected() {
		wsTransport := transport.NewWebSocketTransport(sys.WsConn)
		if err := wsTransport.Request(ctx, action, req, dest); err == nil {
			return nil
		} else if !shouldFallbackToSSH(err) {
			return err
		} else if shouldCloseWebSocket(err) {
			sys.closeWebSocketConnection()
		}
	}

	// Fall back to SSH if WebSocket fails
	sshTransport, err := sys.getSSHTransport()
	if err != nil {
		return err
	}
	return sshTransport.RequestWithRetry(ctx, action, req, dest, 1)
}

func shouldFallbackToSSH(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, gws.ErrConnClosed) {
		return true
	}
	return errors.Is(err, transport.ErrWebSocketNotConnected)
}

func shouldCloseWebSocket(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, gws.ErrConnClosed) || errors.Is(err, transport.ErrWebSocketNotConnected)
}

// getSSHTransport returns the system's SSH transport, creating it on first use.
// The transport owns the only SSH connection to the agent; it is shared by the
// updater and on-demand requests and connects lazily.
// It returns errSSHDisabled if DISABLE_SSH is set on the hub.
func (sys *System) getSSHTransport() (*transport.SSHTransport, error) {
	if sshFallbackDisabled() {
		return nil, errSSHDisabled
	}
	sys.sshMu.Lock()
	defer sys.sshMu.Unlock()
	if sys.sshTransport != nil {
		return sys.sshTransport, nil
	}
	if sys.manager.sshConfig == nil {
		if err := sys.manager.createSSHClientConfig(); err != nil {
			return nil, err
		}
	}
	sys.sshTransport = transport.NewSSHTransport(transport.SSHTransportConfig{
		Host:      sys.Host,
		Port:      sys.Port,
		Config:    sys.manager.sshConfig,
		Timeout:   sessionTimeout,
		OnConnect: sys.onSSHConnect,
	})
	return sys.sshTransport, nil
}

// onSSHConnect resets per-connection state after a new SSH connection is made.
func (sys *System) onSSHConnect(agentVersion semver.Version) {
	sys.setAgentVersion(agentVersion)
}

// getAgentVersion returns the connected agent's version, or zero if unknown.
func (sys *System) getAgentVersion() semver.Version {
	if v := sys.agentVersion.Load(); v != nil {
		return *v
	}
	return semver.Version{}
}

// setAgentVersion records the connected agent's version.
func (sys *System) setAgentVersion(v semver.Version) {
	sys.agentVersion.Store(&v)
}

// fetchDataFromAgent attempts to fetch data from the agent, prioritizing WebSocket if available.
// Each fetch decodes into a new struct: CBOR leaves fields the agent omits
// untouched, and real-time and regular updates may fetch concurrently.
func (sys *System) fetchDataFromAgent(options common.DataRequestOptions) (*system.CombinedData, error) {
	if sys.WsConn != nil && sys.WsConn.IsConnected() {
		wsData, err := sys.fetchDataViaWebSocket(options)
		if err == nil {
			return wsData, nil
		}
		// A slow collection doesn't mean the connection is broken. Closing it
		// would force the agent into a reconnect loop, so only report the error.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		// close the WebSocket connection if error and try SSH
		sys.closeWebSocketConnection()
	}

	sshData, err := sys.fetchDataViaSSH(options)
	if err != nil {
		return nil, err
	}
	return sshData, nil
}

// wsDataRequestTimeout bounds how long to wait for stats over WebSocket. It stays
// under the 5s polling interval so one slow poll never overlaps the next one.
var wsDataRequestTimeout = 4 * time.Second

// maxAgentSilence is how long a WebSocket agent may go without sending anything
// before failed polls stop keeping its connection alive. An agent whose
// collection runs past its own 120s deadline drops the connection itself, so a
// live agent always answers within interval + 120s.
var maxAgentSilence = 3 * time.Minute

func (sys *System) fetchDataViaWebSocket(options common.DataRequestOptions) (*system.CombinedData, error) {
	if sys.WsConn == nil || !sys.WsConn.IsConnected() {
		return nil, errors.New("no websocket connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), wsDataRequestTimeout)
	defer cancel()
	wsTransport := transport.NewWebSocketTransport(sys.WsConn)
	data := &system.CombinedData{}
	if err := wsTransport.Request(ctx, common.GetData, options, data); err != nil {
		return nil, err
	}
	return data, nil
}

func MakeStableHashId(strings ...string) string {
	hash := fnv.New32a()
	for _, str := range strings {
		hash.Write([]byte(str))
	}
	return fmt.Sprintf("%x", hash.Sum32())
}

// fetchDataViaSSH handles fetching data using SSH.
func (sys *System) fetchDataViaSSH(options common.DataRequestOptions) (*system.CombinedData, error) {
	data := &system.CombinedData{}
	err := sys.runSSHOperation(1, func(session *ssh.Session) (bool, error) {
		stdout, err := session.StdoutPipe()
		if err != nil {
			return false, err
		}
		stdin, stdinErr := session.StdinPipe()
		if err := session.Shell(); err != nil {
			return false, err
		}

		// reset in case of retry after a partial decode
		*data = system.CombinedData{}

		if sys.getAgentVersion().GTE(argus.MinVersionAgentResponse) && stdinErr == nil {
			req := common.HubRequest[any]{Action: common.GetData, Data: options}
			_ = cbor.NewEncoder(stdin).Encode(req)
			_ = stdin.Close()

			var resp common.AgentResponse
			if decErr := cbor.NewDecoder(stdout).Decode(&resp); decErr == nil && resp.SystemData != nil {
				*data = *resp.SystemData
				if err := session.Wait(); err != nil {
					return false, err
				}
				return false, nil
			}
		}

		var decodeErr error
		if sys.getAgentVersion().GTE(argus.MinVersionCbor) {
			decodeErr = cbor.NewDecoder(stdout).Decode(data)
		} else {
			decodeErr = json.NewDecoder(stdout).Decode(data)
		}

		if decodeErr != nil {
			return true, decodeErr
		}

		if err := session.Wait(); err != nil {
			return false, err
		}

		return false, nil
	})
	if err != nil {
		return nil, err
	}

	return data, nil
}

// runSSHOperation opens a session on the system's SSH connection and executes
// the provided operation. The operation can request a retry by returning true
// as the first return value.
func (sys *System) runSSHOperation(retries int, operation func(*ssh.Session) (bool, error)) error {
	sshTransport, err := sys.getSSHTransport()
	if err != nil {
		return err
	}
	for attempt := 0; attempt <= retries; attempt++ {
		// A down system may still hold a dead connection, so always re-dial.
		if sys.GetStatus() == down {
			sshTransport.Close()
		}
		client, err := sshTransport.Connect(sys.ctx)
		if err != nil {
			return err
		}

		session, err := sshTransport.NewSession(sys.ctx, client)
		if err != nil {
			if attempt >= retries {
				return err
			}
			sys.manager.hub.Logger().Warn("Session closed. Retrying...", "host", sys.Host, "port", sys.Port, "err", err)
			sshTransport.CloseClient(client)
			continue
		}

		// Bound the whole operation. A half-open TCP connection (a dead peer that
		// never sends RST/FIN) or a wedged agent that accepts the session but
		// never writes a response would otherwise block the read forever. Because
		// StartUpdater runs update() synchronously on its ticker, that stalls the
		// per-system updater indefinitely with no error and no re-dial until the
		// hub is restarted (issue #2041). On timeout we tear down the connection
		// so the blocked read unwinds and the system is re-dialed on the next tick.
		retry, opErr := runWithTimeout(sshOperationTimeout, func() (bool, error) {
			defer session.Close()
			return operation(session)
		}, func() { sshTransport.CloseClient(client) })

		if opErr == nil {
			return nil
		}

		if retry {
			sshTransport.CloseClient(client)
			if attempt < retries {
				continue
			}
		}

		return opErr
	}

	return fmt.Errorf("ssh operation failed")
}

// sshOperationTimeout bounds a single SSH data exchange (send request, read
// response, wait for the remote command to exit). It is more generous than the
// session-creation timeout to tolerate briefly slow agents, but is kept well
// under the collection interval so a stalled connection is detected and
// re-dialed within one cycle (see issue #2041).
const sshOperationTimeout = 20 * time.Second

// runWithTimeout runs op in a goroutine and returns its result, or, if op does
// not finish within timeout, calls onTimeout (used to tear down the connection
// so a blocked op can unwind) and returns a retryable timeout error. This
// guarantees the caller can never block indefinitely on a dead SSH connection.
func runWithTimeout(timeout time.Duration, op func() (bool, error), onTimeout func()) (retry bool, err error) {
	type opResult struct {
		retry bool
		err   error
	}
	// Buffered so the op goroutine never leaks even when we return on timeout.
	done := make(chan opResult, 1)
	go func() {
		r, e := op()
		done <- opResult{retry: r, err: e}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case res := <-done:
		return res.retry, res.err
	case <-timer.C:
		if onTimeout != nil {
			onTimeout()
		}
		return true, fmt.Errorf("ssh operation timed out after %s", timeout)
	}
}

// closeSSHConnection closes the SSH connection but keeps the system in the manager
func (sys *System) closeSSHConnection() {
	sys.sshMu.Lock()
	sshTransport := sys.sshTransport
	sys.sshMu.Unlock()
	if sshTransport != nil {
		sshTransport.Close()
	}
}

// closeWebSocketConnection closes the WebSocket connection but keeps the system in the manager
// to allow updating via SSH. It will be removed if the WS connection is re-established.
// The system will be set as down a few seconds later if the connection is not re-established.
func (sys *System) closeWebSocketConnection() {
	if sys.WsConn != nil {
		sys.WsConn.Close(nil)
	}
}

// getJitter returns a channel that will be triggered after a random delay
// between 51% and 95% of the interval.
// This is used to stagger the initial WebSocket connections to prevent clustering.
func getJitter() <-chan time.Time {
	minPercent := 51
	maxPercent := 95
	jitterRange := maxPercent - minPercent
	msDelay := (interval * minPercent / 100) + rand.Intn(interval*jitterRange/100)
	return time.After(time.Duration(msDelay) * time.Millisecond)
}
