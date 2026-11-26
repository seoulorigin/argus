package systems

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"argus/internal/hub/ws"

	"argus/internal/entities/monitor"
	"argus/internal/entities/probe"
	"argus/internal/entities/system"

	"argus/internal/common"

	"argus"

	"ganigeorgiev.com/pocketbase/dbx"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/store"
	"github.com/blang/semver"
	"golang.org/x/crypto/ssh"
)

// System status constants
const (
	up      string = "up"      // System is online and responding
	down    string = "down"    // System is offline or not responding
	paused  string = "paused"  // System monitoring is paused
	pending string = "pending" // System is waiting on initial connection result

	// interval is the collection and judgement period in milliseconds (5 seconds)
	interval int = 5_000

	// downAfterFailures is the number of consecutive failed polls after which a
	// node is declared down (3 x 5s = 15s without a successful poll).
	downAfterFailures = 3

	// sessionTimeout is the maximum time to wait for SSH connections
	sessionTimeout = 4 * time.Second
)

// errSystemExists is returned when attempting to add a system that already exists
var errSystemExists = errors.New("system exists")

// SystemManager manages a collection of monitored systems and their connections.
// It handles system lifecycle, status updates, and maintains both SSH and WebSocket connections.
type SystemManager struct {
	updaters            sync.WaitGroup                // Tracks updater completion independently of store membership.
	hub                 hubLike                       // Hub interface for database and alert operations
	systems             *store.Store[string, *System] // Thread-safe store of active systems
	sshConfig           *ssh.ClientConfig             // SSH client configuration for system connections
	realtimeMutex       sync.Mutex                    // Protects all realtime worker and subscription state
	activeSubscriptions map[string]*subscriptionInfo  // Realtime subscriptions keyed by system ID
	realtimeWorkerStop  chan struct{}                 // Stops the current realtime worker generation
	realtimeWorkerRun   bool                          // Whether a realtime worker has been started
	ctx                 context.Context               // Cancelled when the app terminates
	cancel              context.CancelFunc            // Cancels ctx and all child system contexts
	// PodName maps a Pod UID reported by an agent to "namespace/name". Set once at
	// startup, before systems are initialized. Nil when the hub has no Kubernetes view.
	PodName func(uid string) (string, bool)
	// PodHostNetwork reports whether a Pod shares its node's network namespace. Such a
	// Pod's traffic counters are the whole node's, so its bandwidth is not reported.
	PodHostNetwork func(uid string) bool
	// ProbeTargets returns the service probes every agent should run. known is false
	// while the hub has no cluster view yet, so agents keep their current targets.
	ProbeTargets func() (targets []probe.Target, known bool)
}

// hubLike defines the interface requirements for the hub dependency.
// It extends core.App with system-specific functionality.
type hubLike interface {
	core.App
	GetSSHKey(dataDir string) (ssh.Signer, error)
}

// NewSystemManager creates a new SystemManager instance with the provided hub.
// The hub must implement the hubLike interface to provide database and alert functionality.
func NewSystemManager(hub hubLike) *SystemManager {
	sm := &SystemManager{
		systems:             store.New(map[string]*System{}),
		hub:                 hub,
		activeSubscriptions: make(map[string]*subscriptionInfo),
	}
	sm.ctx, sm.cancel = context.WithCancel(context.Background())
	return sm
}

// GetSystem returns a system by ID from the store
func (sm *SystemManager) GetSystem(systemID string) (*System, error) {
	sys, ok := sm.systems.GetOk(systemID)
	if !ok {
		return nil, fmt.Errorf("system not found")
	}
	return sys, nil
}

// Initialize sets up the system manager by binding event hooks and starting existing systems.
// It configures SSH client settings and begins monitoring all non-paused systems from the database.
// Systems are started with staggered delays to prevent overwhelming the hub during startup.
func (sm *SystemManager) Initialize() error {
	sm.bindEventHooks()

	// Initialize SSH client configuration
	err := sm.createSSHClientConfig()
	if err != nil {
		return err
	}

	// Load existing systems from database (excluding paused ones)
	var systems []*System
	err = sm.hub.DB().NewQuery("SELECT id, host, port, status FROM systems WHERE status != 'paused'").All(&systems)
	if err != nil || len(systems) == 0 {
		return err
	}

	// Start systems in background with staggered timing
	go func() {
		// Calculate staggered delay between system starts (max 2 seconds per system)
		delta := interval / max(1, len(systems))
		delta = min(delta, 2_000)
		sleepTime := time.Duration(delta) * time.Millisecond

		for _, system := range systems {
			if !waitForContext(sm.ctx, sleepTime) {
				return
			}
			_ = sm.AddSystem(system)
		}
	}()
	return nil
}

// bindEventHooks registers event handlers for system and fingerprint record changes.
// These hooks ensure the system manager stays synchronized with database changes.
func (sm *SystemManager) bindEventHooks() {
	sm.hub.OnRecordCreate("systems").BindFunc(sm.onRecordCreate)
	sm.hub.OnRecordAfterCreateSuccess("systems").BindFunc(sm.onRecordAfterCreateSuccess)
	sm.hub.OnRecordUpdate("systems").BindFunc(sm.onRecordUpdate)
	sm.hub.OnRecordAfterUpdateSuccess("systems").BindFunc(sm.onRecordAfterUpdateSuccess)
	sm.hub.OnRecordAfterDeleteSuccess("systems").BindFunc(sm.onRecordAfterDeleteSuccess)
	sm.hub.OnRecordAfterUpdateSuccess("fingerprints").BindFunc(sm.onTokenRotated)
	sm.hub.OnRealtimeSubscribeRequest().BindFunc(sm.onRealtimeSubscribeRequest)
	sm.hub.OnRealtimeConnectRequest().BindFunc(sm.onRealtimeConnectRequest)
	sm.hub.OnTerminate().BindFunc(sm.onTerminate)
}

// onTerminate cancels SystemManager context on app shutdown
func (sm *SystemManager) onTerminate(e *core.TerminateEvent) error {
	sm.cancel()
	sm.stopRealtimeWorker()
	return e.Next()
}

// onTokenRotated handles fingerprint token rotation events.
// When a system's authentication token is rotated, any existing WebSocket connection
// must be closed to force re-authentication with the new token.
func (sm *SystemManager) onTokenRotated(e *core.RecordEvent) error {
	systemID := e.Record.GetString("system")
	system, ok := sm.systems.GetOk(systemID)
	if !ok {
		return e.Next()
	}
	// No need to close connection if not connected via websocket
	if system.WsConn == nil {
		return e.Next()
	}
	system.setDown(nil)
	sm.RemoveSystem(systemID)
	return e.Next()
}

// onRecordCreate is called before a new system record is committed to the database.
// It initializes the record with default values: empty info and pending status.
func (sm *SystemManager) onRecordCreate(e *core.RecordEvent) error {
	e.Record.Set("info", system.Info{})
	e.Record.Set("status", pending)
	return e.Next()
}

// onRecordAfterCreateSuccess is called after a new system record is successfully created.
// It adds the new system to the manager to begin monitoring.
func (sm *SystemManager) onRecordAfterCreateSuccess(e *core.RecordEvent) error {
	if err := sm.AddRecord(e.Record, nil); err != nil {
		e.App.Logger().Error("Error adding record", "err", err)
	}
	return e.Next()
}

// onRecordUpdate is called before a system record is updated in the database.
// It clears system info when the status is changed to paused.
func (sm *SystemManager) onRecordUpdate(e *core.RecordEvent) error {
	if e.Record.GetString("status") == paused {
		var prevInfo system.Info
		e.Record.UnmarshalJSONField("info", &prevInfo)
		e.Record.Set("info", system.Info{AgentVersion: prevInfo.AgentVersion})
	}
	return e.Next()
}

// onRecordAfterUpdateSuccess handles system record updates after they're committed to the database.
// It manages system lifecycle based on status changes and triggers appropriate alerts.
// Status transitions are handled as follows:
// - paused: Closes SSH connection and deactivates alerts
// - pending: Starts monitoring (reuses WebSocket if available)
// - up: Triggers system alerts
// - down: Cancels pending container alerts and triggers status change alerts
func (sm *SystemManager) onRecordAfterUpdateSuccess(e *core.RecordEvent) error {
	newStatus := e.Record.GetString("status")
	system, ok := sm.systems.GetOk(e.Record.Id)
	if ok {
		system.swapStatus(newStatus)
	}

	switch newStatus {
	case paused:
		if ok {
			// Pause monitoring but keep system in manager for potential resume
			system.closeSSHConnection()
		}
		return e.Next()
	case pending:
		// Resume monitoring, preferring existing WebSocket connection
		if ok && system.WsConn != nil {
			go system.update()
			return e.Next()
		}
		// Start new monitoring session
		if err := sm.AddRecord(e.Record, nil); err != nil {
			e.App.Logger().Error("Error adding record", "err", err)
		}
		return e.Next()
	}

	// Handle systems not in manager
	if !ok {
		return sm.AddRecord(e.Record, nil)
	}

	return e.Next()
}

// onRecordAfterDeleteSuccess is called after a system record is successfully deleted.
// It removes the system from the manager and cleans up all associated resources.
func (sm *SystemManager) onRecordAfterDeleteSuccess(e *core.RecordEvent) error {
	sm.RemoveSystem(e.Record.Id)
	return e.Next()
}

// AddSystem adds a system to the manager and starts monitoring it.
// It validates required fields, initializes the system context, and starts the update goroutine.
// Returns error if a system with the same ID already exists.
func (sm *SystemManager) AddSystem(sys *System) error {
	if sm.systems.Has(sys.Id) {
		return errSystemExists
	}
	if sys.Id == "" || sys.Host == "" {
		return errors.New("system missing required fields")
	}

	// Initialize system for monitoring
	sys.manager = sm
	sys.ctx, sys.cancel = sys.getContext(sm.ctx)
	sys.data = &system.CombinedData{}
	sm.systems.Set(sys.Id, sys)

	// Start monitoring in background
	sm.updaters.Go(sys.StartUpdater)
	return nil
}

// RemoveSystem removes a system from the manager and cleans up all associated resources.
// It cancels the system's context, closes all connections, and removes it from the store.
// Returns an error if the system is not found.
func (sm *SystemManager) RemoveSystem(systemID string) error {
	system, ok := sm.systems.GetOk(systemID)
	if !ok {
		return errors.New("system not found")
	}

	// Stop the update goroutine
	if system.cancel != nil {
		system.cancel()
	}

	// Clean up all connections
	system.closeSSHConnection()
	system.closeWebSocketConnection()
	sm.systems.Remove(systemID)
	return nil
}

// AddRecord creates a System instance from a database record and adds it to the manager.
// If a system with the same ID already exists, it's removed first to ensure clean state.
// If no system instance is provided, a new one is created.
// This method is typically called when systems are created or their status changes to pending.
func (sm *SystemManager) AddRecord(record *core.Record, system *System) (err error) {
	// Remove existing system to ensure clean state
	if sm.systems.Has(record.Id) {
		_ = sm.RemoveSystem(record.Id)
	}

	// Create new system if none provided
	if system == nil {
		system = sm.NewSystem(record.Id)
	}

	// Populate system from record
	system.swapStatus(record.GetString("status"))
	system.Name = record.GetString("name")
	system.Host = record.GetString("host")
	system.Port = record.GetString("port")

	return sm.AddSystem(system)
}

// AddWebSocketSystem creates and adds a system with an established WebSocket connection.
// This method is called when an agent connects via WebSocket with valid authentication.
// The system is immediately added to monitoring with the provided connection and version info.
func (sm *SystemManager) AddWebSocketSystem(systemId string, agentVersion semver.Version, wsConn *ws.WsConn) error {
	systemRecord, err := sm.hub.FindRecordById("systems", systemId)
	if err != nil {
		return err
	}

	system := sm.NewSystem(systemId)
	system.WsConn = wsConn
	system.setAgentVersion(agentVersion)
	system.monitorsNeedSync.Store(true)

	if err := sm.AddRecord(systemRecord, system); err != nil {
		return err
	}

	// Sync network monitors to the newly connected agent
	go system.syncPendingNetworkMonitors()

	return nil
}

// GetMonitorConfigsForSystem returns all enabled monitor configs for a system.
func (sm *SystemManager) GetMonitorConfigsForSystem(systemID string) ([]monitor.Config, error) {
	var configs []monitor.Config
	err := sm.hub.DB().
		NewQuery("SELECT id, target, protocol, port, interval, server, skipTlsVerify, ipVersion, statusCodes FROM network_monitors WHERE system = {:system} AND enabled = true").
		Bind(dbx.Params{"system": systemID}).
		All(&configs)
	return configs, err
}

// createSSHClientConfig initializes the SSH client configuration for connecting to an agent's server
func (sm *SystemManager) createSSHClientConfig() error {
	privateKey, err := sm.hub.GetSSHKey("")
	if err != nil {
		return err
	}

	sm.sshConfig = &ssh.ClientConfig{
		User: "u",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(privateKey),
		},
		Config: ssh.Config{
			Ciphers:      common.DefaultCiphers,
			KeyExchanges: common.DefaultKeyExchanges,
			MACs:         common.DefaultMACs,
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		ClientVersion:   fmt.Sprintf("SSH-2.0-%s_%s", argus.AppName, argus.Version),
		Timeout:         sessionTimeout,
	}
	return nil
}

// waitForContext waits for delay or returns early when ctx is cancelled.
func waitForContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
