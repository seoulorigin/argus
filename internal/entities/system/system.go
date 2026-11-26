package system

// TODO: this is confusing, make common package with common/types common/helpers etc

import (
	"encoding/json"
	"time"

	"argus/internal/entities/container"
	"argus/internal/entities/probe"
)

// Stats are the node-level metrics collected by the agent on every poll.
type Stats struct {
	Cpu               float64              `json:"cpu" cbor:"0,keyasint"`
	MaxCpu            float64              `json:"cpum,omitzero" cbor:"-"`
	MemPct            float64              `json:"mp" cbor:"4,keyasint"`
	NetworkSent       float64              `json:"ns,omitzero" cbor:"16,keyasint,omitzero"`
	NetworkRecv       float64              `json:"nr,omitzero" cbor:"17,keyasint,omitzero"`
	MaxNetworkSent    float64              `json:"nsm,omitzero" cbor:"-"`
	MaxNetworkRecv    float64              `json:"nrm,omitzero" cbor:"-"`
	Bandwidth         [2]uint64            `json:"b,omitzero" cbor:"26,keyasint,omitzero"` // [sent bytes, recv bytes]
	MaxBandwidth      [2]uint64            `json:"bm,omitzero" cbor:"-"`                   // [sent bytes, recv bytes]
	LoadAvg           [3]float64           `json:"la,omitempty" cbor:"28,keyasint"`
	NetworkInterfaces map[string][4]uint64 `json:"ni,omitempty" cbor:"31,keyasint,omitempty"`   // [upload bytes, download bytes, total upload, total download]
	CpuBreakdown      []float64            `json:"cpub,omitempty" cbor:"33,keyasint,omitempty"` // [user, system, iowait, steal, idle, irq, softirq, nice]
	CpuCoresUsage     Uint8Slice           `json:"cpus,omitempty" cbor:"34,keyasint,omitempty"` // per-core busy usage [CPU0..]
	MemBytes          [4]uint64            `json:"mem,omitzero" cbor:"41,keyasint,omitzero"`    // [total, used, buff/cache, reserved] bytes
	MaxMemBytes       uint64               `json:"memm,omitzero" cbor:"-"`                      // max used memory bytes
	SwapBytes         [2]uint64            `json:"swap,omitzero" cbor:"42,keyasint,omitzero"`   // [total, used] bytes
}

// Uint8Slice wraps []uint8 to customize JSON encoding while keeping CBOR efficient.
// JSON: encodes as array of numbers (avoids base64 string).
// CBOR: falls back to default handling for []uint8 (byte string), keeping payload small.
type Uint8Slice []uint8

func (s Uint8Slice) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("null"), nil
	}
	// Convert to wider ints to force array-of-numbers encoding.
	arr := make([]uint16, len(s))
	for i, v := range s {
		arr[i] = uint16(v)
	}
	return json.Marshal(arr)
}

// UnmarshalJSON accepts the numeric array written by MarshalJSON. Without it,
// encoding/json/v2 (used by PocketBase to read record JSON fields) only accepts
// base64 strings for byte slices and rejects the whole value.
func (s *Uint8Slice) UnmarshalJSON(data []byte) error {
	var arr []uint8
	if err := json.Unmarshal(data, &arr); err != nil {
		return err
	}
	*s = arr
	return nil
}

type NetIoStats struct {
	BytesRecv uint64
	BytesSent uint64
	Time      time.Time
	Name      string
}

type Os = uint8

const (
	Linux Os = iota
	Darwin
	Windows
	Freebsd
)

type ConnectionType = uint8

const (
	ConnectionTypeNone ConnectionType = iota
	ConnectionTypeSSH
	ConnectionTypeWebSocket
)

// Info is the small, frequently changing part of a node's state.
type Info struct {
	Threads        int            `json:"t,omitzero" cbor:"3,keyasint,omitempty"`
	Uptime         uint64         `json:"u" cbor:"5,keyasint"`
	Cpu            float64        `json:"cpu" cbor:"6,keyasint"`
	MemPct         float64        `json:"mp" cbor:"7,keyasint"`
	AgentVersion   string         `json:"v" cbor:"10,keyasint"`
	BandwidthBytes uint64         `json:"bb" cbor:"18,keyasint"`
	LoadAvg        [3]float64     `json:"la,omitempty" cbor:"19,keyasint"`
	ConnectionType ConnectionType `json:"ct,omitzero" cbor:"20,keyasint,omitempty,omitzero"`
}

// Data that does not change during process lifetime and is not needed in All Systems table
type Details struct {
	Hostname    string `cbor:"0,keyasint"`
	Kernel      string `cbor:"1,keyasint,omitempty"`
	Cores       int    `cbor:"2,keyasint"`
	Threads     int    `cbor:"3,keyasint"`
	CpuModel    string `cbor:"4,keyasint"`
	Os          Os     `cbor:"5,keyasint"`
	OsName      string `cbor:"6,keyasint"`
	Arch        string `cbor:"7,keyasint"`
	MemoryTotal uint64 `cbor:"9,keyasint"`
	Sockets     int    `cbor:"12,keyasint,omitempty"` // physical CPU packages, 0 if unknown
}

// Final data structure to return to the hub
type CombinedData struct {
	Stats      Stats              `json:"stats" cbor:"0,keyasint"`
	Info       Info               `json:"info" cbor:"1,keyasint"`
	Containers []*container.Stats `json:"container" cbor:"2,keyasint"`
	Details    *Details           `cbor:"4,keyasint,omitempty"`
	// Probes are the service probe samples collected since the previous poll.
	Probes []probe.Sample `cbor:"7,keyasint,omitempty"`
}
