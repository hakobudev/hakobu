package deploy

import (
	"slices"
	"testing"
)

func TestAppContainersDropRawSockets(t *testing.T) {
	_, hostConfig := AppOptions{App: "web", Network: "hakobu_p"}.spec("img")
	if drop, _ := hostConfig["CapDrop"].([]string); !slices.Contains(drop, "NET_RAW") {
		t.Errorf("CapDrop = %v", hostConfig["CapDrop"])
	}
}
