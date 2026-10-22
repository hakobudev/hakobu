package node

import (
	"testing"
)

func TestDockerVolumeNamesAreUnambiguous(t *testing.T) {
	if Volume("a-b", "c") == Volume("a", "b-c") {
		t.Error("different app/volume pairs map to the same Docker volume")
	}
	if got := Volume("web", "data"); got != "hakobu-vol-web_data" {
		t.Errorf("Volume = %q", got)
	}
}
