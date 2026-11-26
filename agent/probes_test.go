package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"argus/internal/entities/probe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hostPort(t *testing.T, addr string) (string, uint16) {
	h, p, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	n, _ := strconv.Atoi(p)
	return h, uint16(n)
}

func TestProbeTCPSuccessAndRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	host, port := hostPort(t, ln.Addr().String())
	r := newProbeRunner()

	s := r.probeOne(context.Background(), probe.Target{ID: "a", Host: host, Port: port})
	assert.True(t, s.OK(), s.Err)
	assert.Positive(t, s.RTTUs)

	ln.Close() // nothing listens any more
	s = r.probeOne(context.Background(), probe.Target{ID: "a", Host: host, Port: port})
	assert.Equal(t, probe.ErrRefused, s.Err)
}

func TestProbeHTTPStatusHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(200)
		case "/missing":
			w.WriteHeader(404)
		case "/boom":
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	host, port := hostPort(t, srv.Listener.Addr().String())
	r := newProbeRunner()
	get := func(path string) probe.Sample {
		return r.probeOne(context.Background(), probe.Target{ID: "w", Kind: probe.HTTP, Host: host, Port: port, Path: path})
	}
	assert.True(t, get("/ok").OK())
	nf := get("/missing")
	assert.True(t, nf.OK(), "a 404 means the service answered; only 5xx is a failure")
	assert.EqualValues(t, 404, nf.Status)
	bad := get("/boom")
	assert.Equal(t, probe.ErrHTTP5xx, bad.Err)
	assert.EqualValues(t, 503, bad.Status)
}

func TestProbeClassifiesDNSAndTimeouts(t *testing.T) {
	r := newProbeRunner()

	r.lookup = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	assert.Equal(t, probe.ErrDNS, r.probeOne(context.Background(), probe.Target{ID: "d", Host: "gone.svc", Port: 80}).Err)

	r.lookup = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true}
	}
	assert.Equal(t, probe.ErrTimeout, r.probeOne(context.Background(), probe.Target{ID: "d", Host: "slow.svc", Port: 80}).Err)

	// a silent network: the dial never returns until the probe deadline
	r.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	s := r.probeOne(context.Background(), probe.Target{ID: "t", Host: "10.1.2.3", Port: 80, TimeoutMs: 150})
	assert.Equal(t, probe.ErrTimeout, s.Err)
	assert.Less(t, time.Since(start), time.Second, "TimeoutMs is honoured")
}

func TestProbeRunnerBuffersAndDrainsOnce(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	host, port := hostPort(t, ln.Addr().String())
	r := newProbeRunner()
	assert.Empty(t, r.Drain())
	r.SetTargets([]probe.Target{{ID: "x", Host: host, Port: port}, {ID: "y", Host: host, Port: port}})
	r.runOnce(context.Background())
	r.runOnce(context.Background())
	got := r.Drain()
	assert.Len(t, got, 4, "two targets over two cycles")
	assert.Empty(t, r.Drain(), "samples are handed over exactly once")

	r.SetTargets(nil)
	r.runOnce(context.Background())
	assert.Empty(t, r.Drain(), "an empty target list stops probing")
}

func TestProbeFromPodSkipsWhenPodIsNotOnThisNode(t *testing.T) {
	r := newProbeRunner()
	r.SetTargets([]probe.Target{{ID: "link|a>b:80", Host: "127.0.0.1", Port: 1, SrcPod: "uid-elsewhere"}})

	r.podNetns = nil
	r.runOnce(context.Background())
	assert.Empty(t, r.Drain(), "no namespace support: no sample")

	r.podNetns = func(uid string) (string, bool) { return "", false }
	r.runOnce(context.Background())
	assert.Empty(t, r.Drain(), "a Pod on another node is not this agent's job")
}

func TestProbeFromPodReportsNetnsFailureSeparately(t *testing.T) {
	r := newProbeRunner()
	r.podNetns = func(uid string) (string, bool) { return "/nonexistent/ns/net", true }
	s, skip := r.probeTarget(context.Background(), probe.Target{ID: "x", Host: "127.0.0.1", Port: 9, SrcPod: "u"})
	assert.False(t, skip)
	assert.Equal(t, probe.ErrNetns, s.Err, "not being able to enter the Pod says nothing about the target")
}

func TestDialInNetnsReachesTargetAndStaysOnOriginalThreadNamespace(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	// entering the namespace we are already in is the only one a test can rely on
	conn, err := dialInNetns(context.Background(), "/proc/self/ns/net", "tcp", ln.Addr().String())
	if errors.Is(err, errNetns) {
		t.Skipf("setns not permitted in this environment: %v", err)
	}
	require.NoError(t, err)
	conn.Close()

	// the process still works normally afterwards, on any thread
	for range 20 {
		c, err := net.Dial("tcp", ln.Addr().String())
		require.NoError(t, err)
		c.Close()
	}
}
