package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"argus/internal/entities/probe"
)

const (
	probeCycle       = 5 * time.Second
	probeConcurrency = 32
	maxBufferedProbe = 2000 // samples kept if the hub stops polling
)

// probeRunner probes the targets the hub hands over, once per cycle, and
// buffers the samples until the hub collects them with its next poll.
type probeRunner struct {
	mu      sync.Mutex
	targets []probe.Target
	buf     []probe.Sample

	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
	client *http.Client
	// podNetns returns the network namespace path of a Pod running on this node.
	podNetns func(uid string) (string, bool)
}

func newProbeRunner() *probeRunner {
	r := &probeRunner{
		dial:   (&net.Dialer{}).DialContext,
		lookup: net.DefaultResolver.LookupIPAddr,
	}
	r.client = &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext:       func(ctx context.Context, n, a string) (net.Conn, error) { return r.dial(ctx, n, a) },
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, // reachability, not certificate checking
		},
	}
	return r
}

// SetTargets replaces the target set. It is a no-op when nothing changed.
func (r *probeRunner) SetTargets(targets []probe.Target) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sameTargets(r.targets, targets) {
		return
	}
	r.targets = append([]probe.Target(nil), targets...)
}

func sameTargets(a, b []probe.Target) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Drain returns the samples collected since the last call.
func (r *probeRunner) Drain() []probe.Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.buf
	r.buf = nil
	return out
}

// Run probes every cycle until ctx is cancelled.
func (r *probeRunner) Run(ctx context.Context) {
	t := time.NewTicker(probeCycle)
	defer t.Stop()
	for {
		r.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *probeRunner) runOnce(ctx context.Context) {
	r.mu.Lock()
	targets := append([]probe.Target(nil), r.targets...)
	r.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	samples := make([]probe.Sample, len(targets))
	skip := make([]bool, len(targets))
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for i, tg := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			samples[i], skip[i] = r.probeTarget(ctx, tg)
		}()
	}
	wg.Wait()

	r.mu.Lock()
	for i, smp := range samples {
		if !skip[i] {
			r.buf = append(r.buf, smp)
		}
	}
	if over := len(r.buf) - maxBufferedProbe; over > 0 {
		r.buf = r.buf[over:]
	}
	r.mu.Unlock()
}

// probeTarget runs a target from where it asks to be run: the node, or the
// network namespace of a Pod on this node. skip is true when the source Pod is
// not on this node, so only the node that hosts it reports.
func (r *probeRunner) probeTarget(ctx context.Context, tg probe.Target) (s probe.Sample, skip bool) {
	if tg.SrcPod == "" {
		return r.probeOne(ctx, tg), false
	}
	if r.podNetns == nil {
		return probe.Sample{}, true
	}
	ns, ok := r.podNetns(tg.SrcPod)
	if !ok {
		return probe.Sample{}, true
	}
	return r.probeIn(ctx, tg, func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialInNetns(ctx, ns, network, addr)
	}), false
}

// probeOne runs a single attempt from the node.
func (r *probeRunner) probeOne(ctx context.Context, tg probe.Target) probe.Sample {
	return r.probeIn(ctx, tg, r.dial)
}

// probeIn runs a single attempt using dial to open connections. Name resolution is done first and is not part
// of the reported RTT, so a slow resolver shows up as a "dns" failure or as
// nothing, not as a slow service.
func (r *probeRunner) probeIn(ctx context.Context, tg probe.Target, dial func(context.Context, string, string) (net.Conn, error)) probe.Sample {
	timeout := probe.DefaultTimeout
	if tg.TimeoutMs > 0 {
		timeout = time.Duration(tg.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	s := probe.Sample{ID: tg.ID, At: time.Now().UnixMilli()}
	ip := tg.Host
	if net.ParseIP(tg.Host) == nil {
		addrs, err := r.lookup(ctx, tg.Host)
		if err != nil || len(addrs) == 0 {
			if err == nil {
				err = &net.DNSError{Err: "no such host", Name: tg.Host, IsNotFound: true}
			}
			s.Err = classify(err)
			if s.Err == probe.ErrOther {
				s.Err = probe.ErrDNS
			}
			return s
		}
		ip = addrs[0].IP.String()
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(int(tg.Port)))

	start := time.Now()
	switch tg.Kind {
	case probe.HTTP:
		scheme := "http"
		if tg.TLS {
			scheme = "https"
		}
		path := tg.Path
		if path == "" {
			path = "/"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+addr+path, nil)
		if err != nil {
			s.Err = probe.ErrOther
			return s
		}
		req.Host = net.JoinHostPort(tg.Host, strconv.Itoa(int(tg.Port)))
		req.Header.Set("User-Agent", "Argus-Probe")
		client := r.client
		if tg.SrcPod != "" { // a per-probe transport, because the dial function differs
			client = &http.Client{
				CheckRedirect: r.client.CheckRedirect,
				Transport: &http.Transport{
					DisableKeepAlives: true,
					DialContext:       dial,
					TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
				},
			}
		}
		resp, err := client.Do(req)
		s.RTTUs = time.Since(start).Microseconds()
		if err != nil {
			s.Err = classify(err)
			return s
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		s.Status = uint16(resp.StatusCode)
		if resp.StatusCode >= 500 {
			s.Err = probe.ErrHTTP5xx
		}
	default:
		conn, err := dial(ctx, "tcp", addr)
		s.RTTUs = time.Since(start).Microseconds()
		if err != nil {
			s.Err = classify(err)
			return s
		}
		_ = conn.Close()
	}
	return s
}

// classify maps a network error to one of the probe.Err* classes.
func classify(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, errNetns):
		return probe.ErrNetns
	case errors.As(err, &dnsErr):
		if dnsErr.IsTimeout {
			return probe.ErrTimeout
		}
		return probe.ErrDNS
	case errors.Is(err, syscall.ECONNREFUSED):
		return probe.ErrRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return probe.ErrReset
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return probe.ErrUnreachable
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return probe.ErrTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return probe.ErrTimeout
	}
	return probe.ErrOther
}
