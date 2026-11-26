package common

import (
	"argus/internal/entities/probe"
	"argus/internal/entities/system"
	"github.com/fxamacker/cbor/v2"
)

type WebSocketAction = uint8

const (
	// Request system data from agent
	GetData WebSocketAction = iota
	// Check the fingerprint of the agent
	CheckFingerprint
	// Add new actions here...
)

// HubRequest defines the structure for requests sent from hub to agent.
type HubRequest[T any] struct {
	Action WebSocketAction `cbor:"0,keyasint"`
	Data   T               `cbor:"1,keyasint,omitempty,omitzero"`
	Id     *uint32         `cbor:"2,keyasint,omitempty"`
}

// AgentResponse defines the structure for responses sent from agent to hub.
type AgentResponse struct {
	Id          *uint32              `cbor:"0,keyasint,omitempty"`
	SystemData  *system.CombinedData `cbor:"1,keyasint,omitempty,omitzero"` // Legacy (<= 0.17)
	Fingerprint *FingerprintResponse `cbor:"2,keyasint,omitempty,omitzero"` // Legacy (<= 0.17)
	Error       string               `cbor:"3,keyasint,omitempty,omitzero"`
	String      *string              `cbor:"4,keyasint,omitempty,omitzero"` // Legacy (<= 0.17)
	// Data is the generic response payload for new endpoints (0.18+)
	Data cbor.RawMessage `cbor:"7,keyasint,omitempty,omitzero"`
}

type FingerprintRequest struct {
	Signature   []byte `cbor:"0,keyasint"`
	NeedSysInfo bool   `cbor:"1,keyasint"` // For universal token system creation
}

type FingerprintResponse struct {
	Fingerprint string `cbor:"0,keyasint"`
	// Optional system info for universal token system creation
	Hostname string `cbor:"1,keyasint,omitzero"`
	Port     string `cbor:"2,keyasint,omitzero"`
	Name     string `cbor:"3,keyasint,omitzero"`
}

type DataRequestOptions struct {
	CacheTimeMs    uint16 `cbor:"0,keyasint"`
	IncludeDetails bool   `cbor:"1,keyasint"`
	// ProbesSet is true when Probes carries the complete target list, which
	// replaces the agent's current one (an empty list stops probing). Every
	// regular poll sets it, so an agent that restarts is told again within 5s.
	ProbesSet bool           `cbor:"2,keyasint,omitempty"`
	Probes    []probe.Target `cbor:"3,keyasint,omitempty"`
}
