// Package transport provides a unified abstraction for hub-agent communication
// over different transports (WebSocket, SSH).
package transport

import (
	"context"
	"errors"
	"fmt"

	"argus/internal/common"
	"argus/internal/entities/system"
	"github.com/fxamacker/cbor/v2"
)

// Transport defines the interface for hub-agent communication.
// Both WebSocket and SSH transports implement this interface.
type Transport interface {
	// Request sends a request to the agent and unmarshals the response into dest.
	// The dest parameter should be a pointer to the expected response type.
	Request(ctx context.Context, action common.WebSocketAction, req any, dest any) error
	// IsConnected returns true if the transport connection is active.
	IsConnected() bool
	// Close terminates the transport connection.
	Close()
}

// UnmarshalResponse unmarshals an AgentResponse into the destination type.
// It first checks the generic Data field (0.19+ agents), then falls back
// to legacy typed fields for backward compatibility with 0.18.0 agents.
func UnmarshalResponse(resp common.AgentResponse, action common.WebSocketAction, dest any) error {
	if dest == nil {
		return errors.New("nil destination")
	}
	// Try generic Data field first (0.19+)
	if len(resp.Data) > 0 {
		if err := cbor.Unmarshal(resp.Data, dest); err != nil {
			return fmt.Errorf("failed to unmarshal generic response data: %w", err)
		}
		return nil
	}
	// Fall back to legacy typed fields for older agents/hubs.
	return unmarshalLegacyResponse(resp, action, dest)
}

// unmarshalLegacyResponse handles legacy responses that use typed fields.
func unmarshalLegacyResponse(resp common.AgentResponse, action common.WebSocketAction, dest any) error {
	switch action {
	case common.GetData:
		d, ok := dest.(*system.CombinedData)
		if !ok {
			return fmt.Errorf("unexpected dest type for GetData: %T", dest)
		}
		if resp.SystemData == nil {
			return errors.New("no system data in response")
		}
		*d = *resp.SystemData
		return nil
	case common.CheckFingerprint:
		d, ok := dest.(*common.FingerprintResponse)
		if !ok {
			return fmt.Errorf("unexpected dest type for CheckFingerprint: %T", dest)
		}
		if resp.Fingerprint == nil {
			return errors.New("no fingerprint in response")
		}
		*d = *resp.Fingerprint
		return nil
	}
	return fmt.Errorf("unsupported action: %d", action)
}
