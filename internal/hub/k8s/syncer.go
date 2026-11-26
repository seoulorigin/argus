package k8s

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Interval is how often the cluster is re-read. It matches the 5s collection
// period, so pod additions and removals show up within one or two cycles.
const Interval = 5 * time.Second

// Fetcher produces cluster snapshots. *Client implements it.
type Fetcher interface {
	Fetch(ctx context.Context) (*Snapshot, error)
}

// Status describes how fresh the hub's cluster view is.
type Status struct {
	LastSuccess time.Time `json:"lastSuccess"`
	LastError   string    `json:"lastError,omitempty"`
	Failures    int       `json:"consecutiveFailures"`
}

// Syncer keeps the latest snapshot. When the Kubernetes API is unreachable it
// keeps serving the last good snapshot and reports the failure in Status.
type Syncer struct {
	fetcher Fetcher
	snap    atomic.Pointer[Snapshot]

	mu     sync.Mutex
	status Status
	byUID  map[string]*Pod
}

func NewSyncer(f Fetcher) *Syncer { return &Syncer{fetcher: f} }

// Snapshot returns the latest good snapshot, or nil before the first success.
func (s *Syncer) Snapshot() *Snapshot { return s.snap.Load() }

func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// PodName returns "namespace/name" for a pod UID known to the latest snapshot.
func (s *Syncer) PodName(uid string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.byUID[uid]; ok {
		return p.FullName(), true
	}
	return "", false
}

// HostNetwork reports whether the pod shares its node's network namespace.
func (s *Syncer) HostNetwork(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byUID[uid]
	return ok && p.HostNetwork
}

// Refresh performs one fetch and swaps in the result.
func (s *Syncer) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, Interval*2)
	defer cancel()
	snap, err := s.fetcher.Fetch(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.status.Failures++
		s.status.LastError = err.Error()
		return err
	}
	if old := s.snap.Load(); old != nil {
		logPodChanges(old, snap)
	}
	s.snap.Store(snap)
	s.byUID = snap.PodByUID()
	s.status = Status{LastSuccess: snap.Taken}
	return nil
}

// Run refreshes until ctx is cancelled. The first refresh happens immediately.
func (s *Syncer) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
			if f := s.Status().Failures; f == 1 || f%12 == 0 {
				slog.Warn("Kubernetes API read failed; keeping last snapshot", "err", err, "consecutiveFailures", f)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func logPodChanges(old, cur *Snapshot) {
	was := old.PodByUID()
	now := cur.PodByUID()
	for uid, p := range now {
		if _, ok := was[uid]; !ok {
			slog.Info("Pod added", "pod", p.FullName(), "node", p.Node)
		}
	}
	for uid, p := range was {
		if _, ok := now[uid]; !ok {
			slog.Info("Pod removed", "pod", p.FullName(), "node", p.Node)
		}
	}
}
