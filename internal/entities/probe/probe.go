// Package probe defines the service reachability probes the hub asks agents to
// run and the samples agents report back.
package probe

import "time"

// Kind is how a target is probed.
type Kind = uint8

const (
	// TCP measures the time to open a TCP connection.
	TCP Kind = iota
	// HTTP measures the time to the response headers of a GET request.
	HTTP
)

// DefaultTimeout is how long a probe may take before it counts as a timeout.
const DefaultTimeout = 2 * time.Second

// Target is one thing the agent must probe every cycle.
type Target struct {
	// ID is the key the samples are reported under, e.g. "shop/cart:7070".
	ID   string `cbor:"0,keyasint"`
	Kind Kind   `cbor:"1,keyasint"`
	// Host is a DNS name or IP. Port is the TCP port.
	Host string `cbor:"2,keyasint"`
	Port uint16 `cbor:"3,keyasint"`
	// Path and TLS are only used for HTTP.
	Path string `cbor:"4,keyasint,omitempty"`
	TLS  bool   `cbor:"5,keyasint,omitempty"`
	// TimeoutMs overrides DefaultTimeout; 0 means the default.
	TimeoutMs uint16 `cbor:"6,keyasint,omitempty"`
	// SrcPod is the UID of a Pod on the agent's node. When set, the connection is
	// opened from inside that Pod's network namespace, so it sees what the Pod
	// sees (NetworkPolicy, Pod-level routing) instead of what the node sees.
	// Agents on other nodes skip the target.
	SrcPod string `cbor:"7,keyasint,omitempty"`
}

// Error classes reported with a failed sample. They say where the failure is:
// the name could not be resolved, nothing listens, the network dropped packets,
// or the service answered with a server error.
const (
	ErrNone        = ""
	ErrTimeout     = "timeout"
	ErrRefused     = "refused"
	ErrDNS         = "dns"
	ErrReset       = "reset"
	ErrUnreachable = "unreachable"
	ErrHTTP5xx     = "http_5xx"
	ErrOther       = "other"
	// ErrNetns means the agent could not enter the source Pod's network namespace
	// (missing capability or security profile). It says nothing about the target.
	ErrNetns = "netns"
)

// Sample is the outcome of one probe attempt.
type Sample struct {
	ID string `cbor:"0,keyasint"`
	// At is when the attempt started, in Unix milliseconds.
	At int64 `cbor:"1,keyasint"`
	// RTTUs is the connect (TCP) or time-to-first-byte (HTTP) time in
	// microseconds. It is only meaningful when Err is empty.
	RTTUs int64 `cbor:"2,keyasint,omitempty"`
	// Err is empty on success, otherwise one of the Err* classes.
	Err string `cbor:"3,keyasint,omitempty"`
	// Status is the HTTP status code, when one was received.
	Status uint16 `cbor:"4,keyasint,omitempty"`
}

// OK reports whether the probe succeeded.
func (s Sample) OK() bool { return s.Err == ErrNone }
