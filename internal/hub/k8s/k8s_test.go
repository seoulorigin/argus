package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuantities(t *testing.T) {
	assert.EqualValues(t, 250, milliCPU("250m"))
	assert.EqualValues(t, 1500, milliCPU("1.5"))
	assert.EqualValues(t, 2000, milliCPU("2"))
	assert.EqualValues(t, 0, milliCPU(""))
	assert.EqualValues(t, 128<<20, bytesOf("128Mi"))
	assert.EqualValues(t, 1<<30, bytesOf("1Gi"))
	assert.EqualValues(t, 500_000_000, bytesOf("500M"))
	assert.EqualValues(t, 129_000_000, bytesOf("129e6"))
	assert.EqualValues(t, 4096, bytesOf("4096"))
}

// fakeAPI serves the four list endpoints from mutable JSON documents.
type fakeAPI struct {
	mu    sync.Mutex
	items map[string][]any
	fail  bool
	srv   *httptest.Server
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{items: map[string][]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		all := f.items[r.URL.Path]
		// serve two items per page to exercise pagination
		start := 0
		if c := r.URL.Query().Get("continue"); c != "" {
			fmt.Sscanf(c, "%d", &start)
		}
		end := min(start+2, len(all))
		page := map[string]any{"items": all[start:end], "metadata": map[string]any{}}
		if end < len(all) {
			page["metadata"] = map[string]any{"continue": fmt.Sprint(end)}
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) set(path string, items ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[path] = items
}

func (f *fakeAPI) client(t *testing.T) *Client {
	tok := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tok, []byte("t0k3n\n"), 0o600))
	t.Setenv("K8S_API_URL", f.srv.URL)
	t.Setenv("K8S_TOKEN_FILE", tok)
	c, err := NewInClusterClient()
	require.NoError(t, err)
	require.NotNil(t, c)
	return c
}

func pod(uid, ns, name, node string, labels map[string]string, ready bool) map[string]any {
	cond := "False"
	if ready {
		cond = "True"
	}
	return map[string]any{
		"metadata": map[string]any{"uid": uid, "namespace": ns, "name": name, "labels": labels,
			"ownerReferences": []any{map[string]any{"kind": "ReplicaSet", "name": name[:len(name)-6]}}},
		"spec": map[string]any{"nodeName": node, "containers": []any{
			map[string]any{"resources": map[string]any{
				"requests": map[string]any{"cpu": "100m", "memory": "64Mi"},
				"limits":   map[string]any{"cpu": "200m", "memory": "128Mi"}}},
			map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": "50m"}}},
		}},
		"status": map[string]any{"phase": "Running", "podIP": "10.0.0.5",
			"conditions": []any{map[string]any{"type": "Ready", "status": cond}},
			"containerStatuses": []any{map[string]any{"restartCount": 3,
				"state": map[string]any{"waiting": map[string]any{"Reason": "CrashLoopBackOff"}}}}},
	}
}

func seed(f *fakeAPI) {
	f.set("/api/v1/nodes",
		map[string]any{"metadata": map[string]any{"name": "node-a", "labels": map[string]any{"node-role.kubernetes.io/control-plane": ""}},
			"status": map[string]any{"capacity": map[string]any{"cpu": "4", "memory": "8Gi"},
				"conditions": []any{map[string]any{"type": "Ready", "status": "True"}, map[string]any{"type": "DiskPressure", "status": "True"}},
				"addresses":  []any{map[string]any{"type": "InternalIP", "address": "192.168.0.10"}}}},
		map[string]any{"metadata": map[string]any{"name": "node-b"},
			"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}},
	)
	f.set("/api/v1/pods",
		pod("u1", "shop", "cart-5d8f7c-abcde", "node-a", map[string]string{"app": "cart"}, true),
		pod("u2", "shop", "cart-5d8f7c-fghij", "node-b", map[string]string{"app": "cart"}, false),
		pod("u3", "shop", "redis-7b9d-klmno", "node-a", map[string]string{"app": "redis"}, true),
	)
	f.set("/api/v1/services",
		map[string]any{"metadata": map[string]any{"namespace": "shop", "name": "cart"},
			"spec": map[string]any{"type": "ClusterIP", "clusterIP": "10.96.0.1", "selector": map[string]any{"app": "cart"},
				"ports": []any{map[string]any{"port": 7070, "protocol": "TCP"}}}},
		map[string]any{"metadata": map[string]any{"namespace": "shop", "name": "orphan"},
			"spec": map[string]any{"selector": map[string]any{"app": "ghost"}}},
	)
	f.set("/apis/discovery.k8s.io/v1/endpointslices",
		map[string]any{"metadata": map[string]any{"namespace": "shop", "labels": map[string]any{"kubernetes.io/service-name": "cart"}},
			"endpoints": []any{
				map[string]any{"conditions": map[string]any{"ready": true}, "targetRef": map[string]any{"kind": "Pod", "uid": "u1"}},
				map[string]any{"conditions": map[string]any{"ready": false}, "targetRef": map[string]any{"kind": "Pod", "uid": "u2"}},
			}},
	)
}

func TestFetchBuildsSnapshot(t *testing.T) {
	f := newFakeAPI(t)
	seed(f)
	snap, err := f.client(t).Fetch(context.Background())
	require.NoError(t, err)

	require.Len(t, snap.Pods, 3, "three pods across two pages")
	require.Len(t, snap.Nodes, 2)
	assert.True(t, snap.Nodes[0].Ready)
	assert.True(t, snap.Nodes[0].ControlPlane)
	assert.Equal(t, "DiskPressure", snap.Nodes[0].ConditionsDown)
	assert.EqualValues(t, 4000, snap.Nodes[0].CPUMilli)
	assert.False(t, snap.Nodes[1].Ready)

	byUID := snap.PodByUID()
	cart := byUID["u1"]
	assert.Equal(t, "shop/cart-5d8f7c-abcde", cart.FullName())
	assert.Equal(t, "Deployment/cart", cart.Owner)
	assert.EqualValues(t, 3, cart.Restarts)
	assert.Equal(t, "CrashLoopBackOff", cart.Reason)
	assert.EqualValues(t, 150, cart.CPURequestMilli)
	assert.EqualValues(t, 0, cart.CPULimitMilli, "a container without a limit makes the pod limit unbounded")
	assert.False(t, byUID["u2"].Ready)

	var cartSvc, orphan Service
	for _, s := range snap.Services {
		switch s.Name {
		case "cart":
			cartSvc = s
		case "orphan":
			orphan = s
		}
	}
	assert.Equal(t, []string{"u1", "u2"}, cartSvc.PodUIDs)
	assert.True(t, cartSvc.HasSlices)
	assert.Equal(t, 1, cartSvc.ReadyEndpoints)
	assert.Equal(t, 2, cartSvc.TotalEndpoints)
	assert.Equal(t, []string{"u1"}, cartSvc.ReadyPodUIDs)
	assert.Empty(t, orphan.PodUIDs)
	assert.False(t, orphan.HasSlices, "no EndpointSlice: counts must not be read as zero ready")
}

func TestSyncerReflectsPodChangesAndSurvivesOutage(t *testing.T) {
	f := newFakeAPI(t)
	seed(f)
	s := NewSyncer(f.client(t))
	ctx := context.Background()
	require.NoError(t, s.Refresh(ctx))
	name, ok := s.PodName("u3")
	require.True(t, ok)
	assert.Equal(t, "shop/redis-7b9d-klmno", name)

	// a pod is deleted and another one appears
	f.set("/api/v1/pods",
		pod("u1", "shop", "cart-5d8f7c-abcde", "node-a", map[string]string{"app": "cart"}, true),
		pod("u4", "shop", "cart-5d8f7c-zzzzz", "node-b", map[string]string{"app": "cart"}, true),
	)
	require.NoError(t, s.Refresh(ctx))
	_, ok = s.PodName("u3")
	assert.False(t, ok, "deleted pod is gone after one refresh")
	_, ok = s.PodName("u4")
	assert.True(t, ok, "new pod is known after one refresh")

	// API outage: the last good snapshot stays and the failure is reported
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	require.Error(t, s.Refresh(ctx))
	require.Error(t, s.Refresh(ctx))
	assert.NotNil(t, s.Snapshot())
	_, ok = s.PodName("u4")
	assert.True(t, ok)
	st := s.Status()
	assert.Equal(t, 2, st.Failures)
	assert.NotEmpty(t, st.LastError)

	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
	require.NoError(t, s.Refresh(ctx))
	assert.Equal(t, 0, s.Status().Failures)
}

func TestNoClusterConfigured(t *testing.T) {
	t.Setenv("K8S_API_URL", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	c, err := NewInClusterClient()
	assert.NoError(t, err)
	assert.Nil(t, c)
}

func TestProbeTargets(t *testing.T) {
	snap := &Snapshot{Services: []Service{
		{Namespace: "shop", Name: "cart", ClusterIP: "10.0.0.1", Selector: map[string]string{"a": "b"},
			Ports: []ServicePort{{Port: 53, Protocol: "UDP"}, {Port: 7070, Protocol: "TCP"}, {Port: 8080}}},
		{Namespace: "shop", Name: "web", ClusterIP: "10.0.0.2", Selector: map[string]string{"a": "b"}, Probe: "http:/healthz", ProbePort: "8080",
			Ports: []ServicePort{{Port: 7070, Protocol: "TCP"}, {Port: 8080, Protocol: "TCP"}}},
		{Namespace: "shop", Name: "off", ClusterIP: "10.0.0.3", Selector: map[string]string{"a": "b"}, Probe: "off", Ports: []ServicePort{{Port: 1}}},
		{Namespace: "shop", Name: "headless", ClusterIP: "None", Selector: map[string]string{"a": "b"}, Ports: []ServicePort{{Port: 1}}},
		{Namespace: "shop", Name: "external", ClusterIP: "10.0.0.4", Ports: []ServicePort{{Port: 1}}},
		{Namespace: "shop", Name: "udp-only", ClusterIP: "10.0.0.5", Selector: map[string]string{"a": "b"}, Ports: []ServicePort{{Port: 53, Protocol: "UDP"}}},
	}}
	got := snap.ProbeTargets("")
	require.Len(t, got, 2)
	assert.Equal(t, "shop/cart", got[0].Service)
	assert.Equal(t, "shop/cart:7070", got[0].ID, "first TCP port, UDP skipped")
	assert.Equal(t, "cart.shop.svc.cluster.local", got[0].Host)
	assert.EqualValues(t, 0, got[0].Kind, "tcp by default")
	assert.Equal(t, "shop/web:8080", got[1].ID, "annotation picks the port")
	assert.Equal(t, "/healthz", got[1].Path)
	assert.EqualValues(t, 1, got[1].Kind, "http")
	assert.Equal(t, "web.shop.svc.example.org", snap.ProbeTargets("example.org")[1].Host)
}

func TestParseLinkPairs(t *testing.T) {
	got, err := ParseLinkPairs(" shop/frontend>shop/cart:7070 , shop/cart>data/redis ,,")
	require.NoError(t, err)
	assert.Equal(t, []LinkPair{{From: "shop/frontend", To: "shop/cart", Port: 7070}, {From: "shop/cart", To: "data/redis"}}, got)
	empty, err := ParseLinkPairs("")
	require.NoError(t, err)
	assert.Empty(t, empty)
	for _, bad := range []string{"shop/frontend", "frontend>cart", "a/b>c/d:notaport", "a/b>c/d:0", "a/b>/d"} {
		_, err := ParseLinkPairs(bad)
		assert.Error(t, err, bad)
	}
}

func TestLinkTargetsOnePerRunningSourcePod(t *testing.T) {
	snap := &Snapshot{
		Pods: []Pod{
			{UID: "p1", Namespace: "shop", Name: "web-1", Phase: "Running"},
			{UID: "p2", Namespace: "shop", Name: "web-2", Phase: "Running"},
			{UID: "p3", Namespace: "shop", Name: "web-3", Phase: "Pending"},
			{UID: "p4", Namespace: "shop", Name: "web-4", Phase: "Running", HostNetwork: true},
			{UID: "p5", Namespace: "shop", Name: "web-5", Phase: "Running", Terminating: true},
		},
		Services: []Service{
			{Namespace: "shop", Name: "web", ClusterIP: "10.0.0.1", Selector: map[string]string{"a": "b"}, PodUIDs: []string{"p1", "p2", "p3", "p4", "p5"}, Ports: []ServicePort{{Port: 80}}},
			{Namespace: "shop", Name: "cart", ClusterIP: "10.0.0.2", Selector: map[string]string{"a": "b"}, Probe: "off", Ports: []ServicePort{{Port: 7070, Protocol: "TCP"}}},
		},
	}
	got := snap.LinkTargets([]LinkPair{{From: "shop/web", To: "shop/cart"}, {From: "shop/web", To: "shop/missing"}})
	require.Len(t, got, 2, "only running, non-hostNetwork, non-terminating Pods; unknown services skipped")
	assert.Equal(t, "link|shop/web-1|shop/cart:7070", got[0].ID)
	assert.Equal(t, "10.0.0.2", got[0].Host, "the ClusterIP, so no DNS is needed inside the Pod")
	assert.Equal(t, "p1", got[0].SrcPod)
	assert.Equal(t, "shop/web", got[0].SrcService)
	assert.Equal(t, "shop/cart", got[0].DstService)
	assert.EqualValues(t, 7070, got[0].Port)
}
