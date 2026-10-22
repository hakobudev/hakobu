package deploy

import (
	"strings"
	"testing"
)

func TestPullError(t *testing.T) {
	ok := `{"status":"Pulling from x"}` + "\n" + `{"status":"Digest: sha256:aa"}` + "\n"
	if err := pullError("x", strings.NewReader(ok)); err != nil {
		t.Errorf("a pull that went through: %v", err)
	}
	refused := ok + `{"errorDetail":{"message":"denied"},"error":"denied: requested access to the resource is denied"}` + "\n"
	if err := pullError("x", strings.NewReader(refused)); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("a refused pull: %v", err)
	}
}
