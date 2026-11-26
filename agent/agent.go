// Package agent implements the Argus monitoring agent that collects and serves system metrics.
//
// The agent runs on monitored systems and communicates collected data
// to the Argus hub for centralized monitoring and alerting.
package agent

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"argus"
	"argus/agent/deltatracker"
	"argus/agent/utils"
	"argus/internal/common"
	"argus/internal/entities/system"
	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

const defaultDataCacheTimeMs uint16 = 5_000

type Agent struct {
	sync.Mutex                                                                      // Used to lock agent while collecting data
	debug                     bool                                                  // true if LOG_LEVEL is set to debug
	memCalc                   string                                                // Memory calculation formula
	forceUseCgroup            bool                                                  // Explicitly use the cgroup mount root for CPU accounting
	netInterfaces             map[string]bool                                       // Valid network interfaces
	netIoStats                map[uint16]system.NetIoStats                          // Keeps track of bandwidth usage per cache interval
	netInterfaceDeltaTrackers map[uint16]*deltatracker.DeltaTracker[string, uint64] // Per-cache-time NIC delta trackers
	systemInfo                system.Info                                           // Host system info (dynamic)
	systemDetails             system.Details                                        // Host system details (static, once-per-connection)
	detailsDirty              bool                                                  // Whether system details have changed and need to be resent
	cache                     *systemDataCache                                      // Cache for system stats based on cache time
	connectionManager         *ConnectionManager                                    // Channel to signal connection events
	handlerRegistry           *HandlerRegistry                                      // Registry for routing incoming messages
	server                    *ssh.Server                                           // SSH server
	serverListener            net.Listener                                          // SSH listener, also closed if Serve has not started yet
	serverMu                  sync.Mutex                                            // Guards server and serverListener
	dataDir                   string                                                // Directory for persisting data
	keys                      []gossh.PublicKey                                     // SSH public keys
	podCollector              *podCollector                                         // Reads per-Pod usage from the node cgroup tree (nil off Kubernetes)
	probes                    *probeRunner                                          // Probes the services the hub names
	monitorManager            *MonitorManager                                       // Manages network monitors
	lastHubRequest            atomic.Int64                                          // Wall clock Unix nanoseconds of the last hub stats request
}

// NewAgent creates a new agent with the given data directory for persisting data.
// If the data directory is not set, it will attempt to find the optimal directory.
func NewAgent(dataDir ...string) (agent *Agent, err error) {
	agent = &Agent{
		cache: NewSystemDataCache(),
	}

	// Initialize per-cache-time network tracking structures
	agent.netIoStats = make(map[uint16]system.NetIoStats)
	agent.netInterfaceDeltaTrackers = make(map[uint16]*deltatracker.DeltaTracker[string, uint64])

	agent.dataDir, err = GetDataDir(dataDir...)
	if err != nil {
		slog.Warn("Data directory not found")
	} else {
		slog.Info("Data directory", "path", agent.dataDir)
	}

	agent.memCalc, _ = utils.GetEnv("MEM_CALC")

	if useCgroup, exists := utils.GetEnv("USE_CGROUP"); exists {
		agent.forceUseCgroup = useCgroup == "true"
	}
	agent.initializeCpu()
	agent.podCollector = newPodCollector()
	agent.probes = newProbeRunner()
	if agent.podCollector != nil {
		agent.probes.podNetns = agent.podCollector.NetnsPath
	}

	// Set up slog with a log level determined by the LOG_LEVEL env var
	if logLevelStr, exists := utils.GetEnv("LOG_LEVEL"); exists {
		switch strings.ToLower(logLevelStr) {
		case "debug":
			agent.debug = true
			slog.SetLogLoggerLevel(slog.LevelDebug)
		case "warn":
			slog.SetLogLoggerLevel(slog.LevelWarn)
		case "error":
			slog.SetLogLoggerLevel(slog.LevelError)
		}
	}

	slog.Debug(argus.Version)

	agent.warnIfRootCgroup()

	// initialize system info
	agent.refreshSystemDetails()

	// initialize connection manager
	agent.connectionManager = newConnectionManager(agent)

	// initialize handler registry
	agent.handlerRegistry = NewHandlerRegistry()

	// initialize monitor manager
	agent.monitorManager = newMonitorManager(agent)

	// initialize net io stats
	agent.initializeNetIoStats()

	// if debugging, print stats
	if agent.debug {
		slog.Debug("Stats", "data", agent.gatherStats(common.DataRequestOptions{CacheTimeMs: defaultDataCacheTimeMs, IncludeDetails: true}))
	}

	return agent, nil
}

// markHubRequest records a hub stats request. It doesn't use the agent lock,
// which gatherStats holds through collection.
func (a *Agent) markHubRequest() {
	a.lastHubRequest.Store(time.Now().UnixNano())
}

// hubIdleFor reports whether the hub hasn't requested stats for at least d.
// Wall time counts host suspend toward idle.
func (a *Agent) hubIdleFor(d time.Duration) bool {
	return time.Now().UnixNano()-a.lastHubRequest.Load() >= int64(d)
}

func (a *Agent) gatherStats(options common.DataRequestOptions) *system.CombinedData {
	a.Lock()
	defer a.Unlock()

	if options.ProbesSet && a.probes != nil {
		a.probes.SetTargets(options.Probes)
	}

	cacheTimeMs := options.CacheTimeMs
	data, isCached := a.cache.Get(cacheTimeMs)
	if isCached {
		slog.Debug("Cached data", "cacheTimeMs", cacheTimeMs)
		return data
	}

	*data = system.CombinedData{
		Stats: a.getSystemStats(cacheTimeMs),
		Info:  a.systemInfo,
	}

	// slog.Info("System data", "data", data, "cacheTimeMs", cacheTimeMs)

	if a.podCollector != nil {
		data.Containers = a.podCollector.Collect(cacheTimeMs)
	}

	// Probe samples are handed over exactly once, with the regular poll. Realtime
	// views read other cache intervals and must not consume them.
	if cacheTimeMs == defaultDataCacheTimeMs {
		if a.probes != nil {
			data.Probes = a.probes.Drain()
		}
	}

	if a.monitorManager != nil {
		data.Monitors = a.monitorManager.GetResults(cacheTimeMs)
		slog.Debug("Monitors", "data", data.Monitors)
	}

	a.cache.Set(data, cacheTimeMs)

	return a.attachSystemDetails(data, cacheTimeMs, options.IncludeDetails)
}

// Start initializes and starts the agent with optional WebSocket connection.
// It runs until ctx is cancelled or the process receives SIGINT or SIGTERM.
func (a *Agent) Start(ctx context.Context, serverOptions ServerOptions) error {
	a.keys = serverOptions.Keys
	if a.probes != nil {
		go a.probes.Run(ctx)
	}
	return a.connectionManager.Start(ctx, serverOptions)
}

func (a *Agent) getFingerprint() string {
	return GetFingerprint(a.dataDir, a.systemDetails.Hostname, a.systemDetails.CpuModel)
}
