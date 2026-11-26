//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePodDirName(t *testing.T) {
	tests := []struct {
		name, uid string
		ok        bool
	}{
		{"kubepods-burstable-pod5f1b2c3d_1111_2222_3333_444455556666.slice", "5f1b2c3d-1111-2222-3333-444455556666", true},
		{"kubepods-pod5f1b2c3d_1111_2222_3333_444455556666.slice", "5f1b2c3d-1111-2222-3333-444455556666", true},
		{"kubepods-besteffort-podabcdef12_0000_0000_0000_000000000001.slice", "abcdef12-0000-0000-0000-000000000001", true},
		{"pod5f1b2c3d-1111-2222-3333-444455556666", "5f1b2c3d-1111-2222-3333-444455556666", true},
		{"kubepods-burstable.slice", "", false},
		{"cri-containerd-0123456789abcdef.scope", "", false},
		{"burstable", "", false},
	}
	for _, tc := range tests {
		uid, ok := parsePodDirName(tc.name)
		assert.Equal(t, tc.ok, ok, tc.name)
		assert.Equal(t, tc.uid, uid, tc.name)
	}
}

func TestPodUIDFromCgroupFile(t *testing.T) {
	content := "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod5f1b2c3d_1111_2222_3333_444455556666.slice/cri-containerd-aa.scope\n"
	uid, ok := podUIDFromCgroupFile(content)
	require.True(t, ok)
	assert.Equal(t, "5f1b2c3d-1111-2222-3333-444455556666", uid)
	_, ok = podUIDFromCgroupFile("0::/system.slice/sshd.service\n")
	assert.False(t, ok)
}

type fakeNode struct {
	root, proc string
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func (n fakeNode) pod(t *testing.T, dir string, cpuUsec, memCur, inactive uint64) {
	writeFile(t, filepath.Join(n.root, dir, "cpu.stat"), fmt.Sprintf("usage_usec %d\nuser_usec 1\n", cpuUsec))
	writeFile(t, filepath.Join(n.root, dir, "memory.current"), fmt.Sprintf("%d\n", memCur))
	writeFile(t, filepath.Join(n.root, dir, "memory.stat"), fmt.Sprintf("anon 1\ninactive_file %d\n", inactive))
}

func (n fakeNode) process(t *testing.T, pid int, cgroupPath string, rx, tx uint64) {
	p := filepath.Join(n.proc, fmt.Sprint(pid))
	writeFile(t, filepath.Join(p, "cgroup"), "0::"+cgroupPath+"\n")
	writeFile(t, filepath.Join(p, "net", "dev"),
		"Inter-|   Receive                                                |  Transmit\n"+
			" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"+
			"    lo: 999 1 0 0 0 0 0 0 999 1 0 0 0 0 0 0\n"+
			fmt.Sprintf("  eth0: %d 10 0 0 0 0 0 0 %d 10 0 0 0 0 0 0\n", rx, tx))
}

func newFakeCollector(t *testing.T) (*podCollector, fakeNode, *time.Time) {
	node := fakeNode{root: filepath.Join(t.TempDir(), "cgroup"), proc: filepath.Join(t.TempDir(), "proc")}
	require.NoError(t, os.MkdirAll(node.proc, 0o755))
	writeFile(t, filepath.Join(node.root, "cgroup.controllers"), "cpu memory\n")
	clock := time.Unix(1_000_000, 0)
	c := &podCollector{
		cgroupRoot: node.root, procRoot: node.proc, numCPU: 4,
		prev: map[uint16]map[string]podSample{}, pidCache: map[string]int{}, warned: map[string]bool{},
		now: func() time.Time { return clock },
	}
	return c, node, &clock
}

func TestPodCollectorSystemdDriver(t *testing.T) {
	c, node, clock := newFakeCollector(t)
	const uidA = "5f1b2c3d-1111-2222-3333-444455556666"
	const uidB = "abcdef12-0000-0000-0000-000000000001"
	dirA := "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod5f1b2c3d_1111_2222_3333_444455556666.slice"
	dirB := "kubepods.slice/kubepods-pod" + "abcdef12_0000_0000_0000_000000000001.slice" // guaranteed
	node.pod(t, dirA, 1_000_000, 300<<20, 100<<20)
	node.pod(t, dirB, 0, 50<<20, 0)
	node.process(t, 101, "/"+dirA+"/cri-containerd-aa.scope", 1000, 2000)
	node.process(t, 202, "/"+dirB+"/cri-containerd-bb.scope", 0, 0)
	node.process(t, 303, "/system.slice/sshd.service", 5, 5)

	first := c.Collect(5000)
	require.Len(t, first, 2)
	for _, s := range first {
		assert.Zero(t, s.Cpu, "no previous sample yet")
		assert.Equal(t, [2]uint64{}, s.Bandwidth)
	}

	// 5s later: pod A used 2 CPU-seconds on a 4-CPU node => 2/(5*4)=10%; sent 5000 B, received 2500 B => 1000 B/s, 500 B/s.
	*clock = clock.Add(5 * time.Second)
	node.pod(t, dirA, 3_000_000, 300<<20, 100<<20)
	node.process(t, 101, "/"+dirA+"/cri-containerd-aa.scope", 3500, 7000)
	got := map[string]float64{}
	for _, s := range c.Collect(5000) {
		got[s.Id] = s.Cpu
		switch s.Id {
		case uidA:
			assert.InDelta(t, 10.0, s.Cpu, 0.01)
			assert.InDelta(t, 200.0, s.Mem, 0.01, "working set = current - inactive_file = 200 MiB")
			assert.Equal(t, "burstable", s.Status)
			assert.Equal(t, [2]uint64{1000, 500}, s.Bandwidth, "[sent, recv] bytes/s, loopback excluded")
		case uidB:
			assert.Zero(t, s.Cpu)
			assert.Equal(t, "guaranteed", s.Status)
		}
	}
	assert.Len(t, got, 2)

	// A separate interval keeps its own baseline.
	assert.Len(t, c.Collect(1000), 2)
}

func TestPodCollectorCgroupfsDriverAndPodRemoval(t *testing.T) {
	c, node, clock := newFakeCollector(t)
	dir := "kubepods/besteffort/pod5f1b2c3d-1111-2222-3333-444455556666"
	node.pod(t, dir, 100, 10<<20, 0)
	node.process(t, 7, "/"+dir+"/abc", 1, 1)
	require.Len(t, c.Collect(5000), 1)

	require.NoError(t, os.RemoveAll(filepath.Join(node.root, "kubepods")))
	*clock = clock.Add(5 * time.Second)
	assert.Empty(t, c.Collect(5000), "a Pod that disappears is dropped")
	assert.Empty(t, c.prev[5000], "and its baseline is forgotten")
}

func TestPodCollectorCounterReset(t *testing.T) {
	c, node, clock := newFakeCollector(t)
	dir := "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod5f1b2c3d_1111_2222_3333_444455556666.slice"
	node.pod(t, dir, 9_000_000, 1<<20, 0)
	node.process(t, 5, "/"+dir+"/x", 10_000, 10_000)
	c.Collect(5000)
	*clock = clock.Add(5 * time.Second)
	node.pod(t, dir, 1_000, 1<<20, 0) // counters restarted lower
	node.process(t, 5, "/"+dir+"/x", 10, 10)
	s := c.Collect(5000)[0]
	assert.Zero(t, s.Cpu)
	assert.Equal(t, [2]uint64{}, s.Bandwidth)
}

func TestPodCollectorHostNetworkPodSkipped(t *testing.T) {
	c, node, clock := newFakeCollector(t)
	dir := "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod5f1b2c3d_1111_2222_3333_444455556666.slice"
	node.pod(t, dir, 0, 1<<20, 0)
	node.process(t, 5, "/"+dir+"/x", 1_000, 1_000)
	// both the pod process and pid 1 point at the same network namespace
	for _, pid := range []int{1, 5} {
		require.NoError(t, os.MkdirAll(filepath.Join(node.proc, fmt.Sprint(pid), "ns"), 0o755))
		require.NoError(t, os.Symlink("net:[4026531840]", filepath.Join(node.proc, fmt.Sprint(pid), "ns", "net")))
	}
	c.hostNet = readNSLink(filepath.Join(node.proc, "1", "ns", "net"))
	c.Collect(5000)
	*clock = clock.Add(5 * time.Second)
	node.process(t, 5, "/"+dir+"/x", 9_000_000, 9_000_000)
	assert.Equal(t, [2]uint64{}, c.Collect(5000)[0].Bandwidth)
}

func TestNewPodCollectorDisabledWithoutKubepods(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fs", "cgroup", "cgroup.controllers"), "cpu\n")
	t.Setenv("HOST_SYS", dir)
	assert.Nil(t, newPodCollector())
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "fs", "cgroup", "kubepods.slice"), 0o755))
	assert.NotNil(t, newPodCollector())
}
