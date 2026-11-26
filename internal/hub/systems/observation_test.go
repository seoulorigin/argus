package systems

import (
	"testing"

	"argus/internal/entities/container"
	"argus/internal/entities/system"

	"github.com/stretchr/testify/assert"
)

func TestNameContainersResolvesNamesAndDropsHostNetworkTraffic(t *testing.T) {
	sys := &System{manager: &SystemManager{
		PodName: func(uid string) (string, bool) {
			if uid == "u1" || uid == "u2" {
				return "shop/" + uid, true
			}
			return "", false
		},
		PodHostNetwork: func(uid string) bool { return uid == "u2" },
	}}
	data := &system.CombinedData{Containers: []*container.Stats{
		{Id: "u1", Name: "u1", Bandwidth: [2]uint64{10, 20}},
		{Id: "u2", Name: "u2", Bandwidth: [2]uint64{9000, 9000}}, // hostNetwork: counters are the whole node's
		{Id: "u3", Name: "u3", Bandwidth: [2]uint64{1, 2}},       // unknown to the cluster view
	}}
	sys.nameContainers(data)
	assert.Equal(t, "shop/u1", data.Containers[0].Name)
	assert.Equal(t, [2]uint64{10, 20}, data.Containers[0].Bandwidth)
	assert.Equal(t, [2]uint64{}, data.Containers[1].Bandwidth)
	assert.Equal(t, "u3", data.Containers[2].Name, "unknown pods keep their UID")
	assert.Equal(t, [2]uint64{1, 2}, data.Containers[2].Bandwidth)
}
