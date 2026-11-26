package agent

import (
	"argus/internal/common"
	"argus/internal/entities/system"
	"github.com/fxamacker/cbor/v2"
)

// newAgentResponse creates an AgentResponse for the given payload.
func newAgentResponse(data any, requestID *uint32) common.AgentResponse {
	response := common.AgentResponse{Id: requestID}
	switch v := data.(type) {
	case *system.CombinedData:
		response.SystemData = v
	case *common.FingerprintResponse:
		response.Fingerprint = v
	case string:
		response.String = &v
	default:
		// For unknown types, use the generic Data field
		response.Data, _ = cbor.Marshal(data)
	}
	return response
}
