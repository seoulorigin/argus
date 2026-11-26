//go:build linux

package agent

import (
	"bufio"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"argus/agent/utils"
	"argus/internal/entities/container"
)

// podCollector reads per-Pod CPU, memory and network usage straight from the
// node's cgroup tree (the kubelet creates one cgroup per Pod), so it needs no
// container runtime socket and no Kubernetes API access.
//
// Pods are keyed by their UID, the only identity a cgroup path carries. The hub
// maps the UID to namespace/name using its Kubernetes view.
type podCollector struct {
	cgroupRoot string // cgroup v2 mount, e.g. /host/sys/fs/cgroup
	procRoot   string // node's procfs, e.g. /host/proc
	numCPU     int

	mu       sync.Mutex
	prev     map[uint16]map[string]podSample // per cacheTimeMs, per pod UID
	pidCache map[string]int                  // pod UID -> a pid inside that pod
	hostNet  string                          // node network namespace id
	warned   map[string]bool
	now      func() time.Time // replaced in tests
}

type podSample struct {
	at       time.Time
	cpuUsec  uint64
	rx, tx   uint64
	netValid bool
}

type podDir struct {
	uid  string // canonical, with dashes
	qos  string // guaranteed | burstable | besteffort
	path string
}

// newPodCollector returns nil when the node's cgroup tree has no kubepods
// hierarchy (not a Kubernetes node, or cgroup v1), which disables Pod collection.
func newPodCollector() *podCollector {
	sysRoot := os.Getenv("HOST_SYS")
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	procRoot := os.Getenv("HOST_PROC")
	if procRoot == "" {
		procRoot = "/proc"
	}
	root := filepath.Join(sysRoot, "fs", "cgroup")
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		slog.Info("Pod collection disabled: no cgroup v2 hierarchy", "root", root)
		return nil
	}
	if len(listPodDirs(root)) == 0 {
		if !hasKubepods(root) {
			slog.Info("Pod collection disabled: no kubepods cgroup", "root", root)
			return nil
		}
	}
	slog.Info("Pod collection enabled", "cgroupRoot", root, "procRoot", procRoot)
	c := &podCollector{
		cgroupRoot: root,
		procRoot:   procRoot,
		numCPU:     max(1, runtime.NumCPU()),
		prev:       map[uint16]map[string]podSample{},
		pidCache:   map[string]int{},
		warned:     map[string]bool{},
		now:        time.Now,
	}
	c.hostNet = readNSLink(filepath.Join(procRoot, "1", "ns", "net"))
	return c
}

func hasKubepods(root string) bool {
	for _, n := range []string{"kubepods.slice", "kubepods"} {
		if st, err := os.Stat(filepath.Join(root, n)); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

// listPodDirs finds every Pod cgroup under root for both kubelet cgroup drivers:
//
//	systemd:  kubepods.slice/[kubepods-<qos>.slice/]kubepods[-<qos>]-pod<uid_with_underscores>.slice
//	cgroupfs: kubepods/[<qos>/]pod<uid-with-dashes>
func listPodDirs(root string) []podDir {
	var out []podDir
	scan := func(dir, qos string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if uid, ok := parsePodDirName(e.Name()); ok {
				out = append(out, podDir{uid: uid, qos: qos, path: filepath.Join(dir, e.Name())})
			}
		}
	}
	for _, base := range []string{"kubepods.slice", "kubepods"} {
		b := filepath.Join(root, base)
		if _, err := os.Stat(b); err != nil {
			continue
		}
		scan(b, "guaranteed")
		for _, qos := range []string{"burstable", "besteffort"} {
			for _, name := range []string{"kubepods-" + qos + ".slice", qos} {
				scan(filepath.Join(b, name), qos)
			}
		}
	}
	return out
}

// parsePodDirName extracts the Pod UID from a cgroup directory name.
func parsePodDirName(name string) (string, bool) {
	var uid string
	switch {
	case strings.HasSuffix(name, ".slice"):
		i := strings.LastIndex(name, "-pod")
		if i < 0 {
			return "", false
		}
		uid = strings.TrimSuffix(name[i+len("-pod"):], ".slice")
	case strings.HasPrefix(name, "pod"):
		uid = strings.TrimPrefix(name, "pod")
	default:
		return "", false
	}
	uid = strings.ReplaceAll(uid, "_", "-")
	if len(uid) < 8 {
		return "", false
	}
	return uid, true
}

// Collect returns one entry per Pod found on this node. CPU is a percentage of
// the node's total CPU capacity, memory is the working set in MiB and Bandwidth
// is bytes per second, matching how the dashboard already charts these values.
func (c *podCollector) Collect(cacheTimeMs uint16) []*container.Stats {
	pods := listPodDirs(c.cgroupRoot)
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	prev := c.prev[cacheTimeMs]
	cur := make(map[string]podSample, len(pods))
	pidOf := c.podPIDs(pods)
	result := make([]*container.Stats, 0, len(pods))

	for _, p := range pods {
		cpuUsec, okCPU := readCPUUsage(p.path)
		mem, okMem := readWorkingSet(p.path)
		if !okCPU && !okMem {
			continue // the Pod was removed between listing and reading
		}
		s := podSample{at: now, cpuUsec: cpuUsec}
		if pid, ok := pidOf[p.uid]; ok {
			s.rx, s.tx, s.netValid = c.readPodNet(p.uid, pid)
		}
		cur[p.uid] = s

		st := &container.Stats{Id: p.uid, Name: p.uid, Status: p.qos, Mem: utils.TwoDecimals(utils.BytesToMegabytes(float64(mem)))}
		if old, ok := prev[p.uid]; ok {
			if elapsed := now.Sub(old.at); elapsed > 0 {
				// A counter that went backwards (cgroup recreated) is a new baseline, not a negative rate.
				if s.cpuUsec >= old.cpuUsec {
					capacityUsec := float64(elapsed.Microseconds()) * float64(c.numCPU)
					st.Cpu = utils.TwoDecimals(float64(s.cpuUsec-old.cpuUsec) / capacityUsec * 100)
				}
				if s.netValid && old.netValid && s.tx >= old.tx && s.rx >= old.rx {
					secs := elapsed.Seconds()
					st.Bandwidth = [2]uint64{uint64(float64(s.tx-old.tx) / secs), uint64(float64(s.rx-old.rx) / secs)}
				}
			}
		}
		result = append(result, st)
	}
	c.prev[cacheTimeMs] = cur
	return result
}

func readCPUUsage(dir string) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return 0, false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// readWorkingSet returns memory.current minus inactive file cache, the same
// "working set" definition the kubelet and cAdvisor use.
func readWorkingSet(dir string) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0, false
	}
	cur, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	if stat, err := os.ReadFile(filepath.Join(dir, "memory.stat")); err == nil {
		for line := range strings.SplitSeq(string(stat), "\n") {
			if v, ok := strings.CutPrefix(line, "inactive_file "); ok {
				if inactive, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
					if inactive > cur {
						return 0, true
					}
					return cur - inactive, true
				}
			}
		}
	}
	return cur, true
}

// podPIDs maps each Pod UID to one process id inside it. A process id is needed
// only to reach the Pod's network namespace (/proc/<pid>/net/dev). Cached ids
// are re-checked with a single read; a full /proc scan runs only when some Pod
// has no valid id.
func (c *podCollector) podPIDs(pods []podDir) map[string]int {
	result := make(map[string]int, len(pods))
	var missing []string
	for _, p := range pods {
		if pid, ok := c.pidCache[p.uid]; ok && c.pidInPod(pid, p.uid) {
			result[p.uid] = pid
		} else {
			delete(c.pidCache, p.uid)
			missing = append(missing, p.uid)
		}
	}
	if len(missing) > 0 {
		want := make(map[string]bool, len(missing))
		for _, uid := range missing {
			want[uid] = true
		}
		entries, _ := os.ReadDir(c.procRoot)
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			data, err := os.ReadFile(filepath.Join(c.procRoot, e.Name(), "cgroup"))
			if err != nil {
				continue
			}
			if uid, ok := podUIDFromCgroupFile(string(data)); ok && want[uid] {
				result[uid] = pid
				c.pidCache[uid] = pid
				delete(want, uid)
				if len(want) == 0 {
					break
				}
			}
		}
	}
	return result
}

func (c *podCollector) pidInPod(pid int, uid string) bool {
	data, err := os.ReadFile(filepath.Join(c.procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return false
	}
	got, ok := podUIDFromCgroupFile(string(data))
	return ok && got == uid
}

// podUIDFromCgroupFile finds the Pod UID in a /proc/<pid>/cgroup file ("0::/kubepods.slice/.../kubepods-burstable-pod<uid>.slice/cri-containerd-<id>.scope").
func podUIDFromCgroupFile(content string) (string, bool) {
	for line := range strings.SplitSeq(content, "\n") {
		_, path, found := strings.Cut(line, "::")
		if !found {
			continue
		}
		for part := range strings.SplitSeq(path, "/") {
			if uid, ok := parsePodDirName(part); ok {
				return uid, true
			}
		}
	}
	return "", false
}

// readPodNet sums received and sent bytes over the Pod's non-loopback interfaces.
// Pods that share the node's network namespace (hostNetwork) are skipped because
// their counters would be the whole node's traffic.
func (c *podCollector) readPodNet(uid string, pid int) (rx, tx uint64, ok bool) {
	base := filepath.Join(c.procRoot, strconv.Itoa(pid))
	if ns := readNSLink(filepath.Join(base, "ns", "net")); ns != "" && ns == c.hostNet {
		return 0, 0, false
	}
	f, err := os.Open(filepath.Join(base, "net", "dev"))
	if err != nil {
		if !c.warned[uid] {
			c.warned[uid] = true
			slog.Debug("Pod network counters unreadable", "pod", uid, "err", err)
		}
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		if strings.TrimSpace(name) == "lo" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		r, err1 := strconv.ParseUint(fields[0], 10, 64)
		t, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx += r
		tx += t
		ok = true
	}
	return rx, tx, ok
}

func readNSLink(path string) string {
	s, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return s
}

// NetnsPath returns the network namespace file of a Pod running on this node.
func (c *podCollector) NetnsPath(uid string) (string, bool) {
	onNode := false
	for _, d := range listPodDirs(c.cgroupRoot) {
		if d.uid == uid {
			onNode = true
			break
		}
	}
	if !onNode {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pid, ok := c.podPIDs([]podDir{{uid: uid}})[uid]
	if !ok {
		return "", false
	}
	return filepath.Join(c.procRoot, strconv.Itoa(pid), "ns", "net"), true
}
