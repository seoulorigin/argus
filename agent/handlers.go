package agent

import (
	"errors"
	"fmt"

	"argus/internal/common"
	"argus/internal/entities/monitor"
	"argus/internal/entities/system"
	"github.com/fxamacker/cbor/v2"
	"github.com/lxzan/gws"
)

// HandlerContext provides context for request handlers
type HandlerContext struct {
	Client         *WebSocketClient
	Conn           *gws.Conn // WebSocket that carried this request, if any
	Agent          *Agent
	Request        *common.HubRequest[cbor.RawMessage]
	RequestID      *uint32
	HubVerified    bool
	ConnectionType system.ConnectionType // Transport that carried this request
	// SendResponse abstracts how a handler sends responses (WS or SSH)
	SendResponse func(data any, requestID *uint32) error
}

// RequestHandler defines the interface for handling specific websocket request types
type RequestHandler interface {
	// Handle processes the request and returns an error if unsuccessful
	Handle(hctx *HandlerContext) error
}

// Responder sends handler responses back to the hub (over WS or SSH)
type Responder interface {
	SendResponse(data any, requestID *uint32) error
}

// HandlerRegistry manages the mapping between actions and their handlers
type HandlerRegistry struct {
	handlers map[common.WebSocketAction]RequestHandler
}

// NewHandlerRegistry creates a new handler registry with default handlers
func NewHandlerRegistry() *HandlerRegistry {
	registry := &HandlerRegistry{
		handlers: make(map[common.WebSocketAction]RequestHandler),
	}

	registry.Register(common.GetData, &GetDataHandler{})
	registry.Register(common.CheckFingerprint, &CheckFingerprintHandler{})
	registry.Register(common.SyncNetworkMonitors, &SyncNetworkMonitorsHandler{})

	return registry
}

// Register registers a handler for a specific action type
func (hr *HandlerRegistry) Register(action common.WebSocketAction, handler RequestHandler) {
	hr.handlers[action] = handler
}

// Handle routes the request to the appropriate handler
func (hr *HandlerRegistry) Handle(hctx *HandlerContext) error {
	handler, exists := hr.handlers[hctx.Request.Action]
	if !exists {
		return fmt.Errorf("unknown action: %d", hctx.Request.Action)
	}

	// Check verification requirement - default to requiring verification
	if hctx.Request.Action != common.CheckFingerprint && !hctx.HubVerified {
		return errors.New("hub not verified")
	}

	// Log handler execution for debugging
	// slog.Debug("Executing handler", "action", hctx.Request.Action)

	return handler.Handle(hctx)
}

// GetHandler returns the handler for a specific action
func (hr *HandlerRegistry) GetHandler(action common.WebSocketAction) (RequestHandler, bool) {
	handler, exists := hr.handlers[action]
	return handler, exists
}

////////////////////////////////////////////////////////////////////////////
////////////////////////////////////////////////////////////////////////////

// GetDataHandler handles system data requests
type GetDataHandler struct{}

func (h *GetDataHandler) Handle(hctx *HandlerContext) error {
	var options common.DataRequestOptions
	_ = cbor.Unmarshal(hctx.Request.Data, &options)

	hctx.Agent.markHubRequest()
	sysStats := hctx.Agent.gatherStats(options)
	// Cached stats may be shared by concurrent SSH and WebSocket requests.
	// Set the transport on the response copy, not on the cached data.
	response := *sysStats
	response.Info.ConnectionType = hctx.ConnectionType
	return hctx.SendResponse(&response, hctx.RequestID)
}

////////////////////////////////////////////////////////////////////////////
////////////////////////////////////////////////////////////////////////////

// CheckFingerprintHandler handles authentication challenges
type CheckFingerprintHandler struct{}

func (h *CheckFingerprintHandler) Handle(hctx *HandlerContext) error {
	return hctx.Client.handleAuthChallenge(hctx.Request, hctx.RequestID, hctx.Conn)
}

////////////////////////////////////////////////////////////////////////////
////////////////////////////////////////////////////////////////////////////

// SyncNetworkMonitorsHandler handles monitor configuration sync from hub
type SyncNetworkMonitorsHandler struct{}

func (h *SyncNetworkMonitorsHandler) Handle(hctx *HandlerContext) error {
	var req monitor.SyncRequest
	if err := cbor.Unmarshal(hctx.Request.Data, &req); err != nil {
		return err
	}
	resp, err := hctx.Agent.monitorManager.HandleSyncRequest(req)
	if err != nil {
		return err
	}
	return hctx.SendResponse(resp, hctx.RequestID)
}
