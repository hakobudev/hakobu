package ops

import (
	"strings"
	"testing"
)

func TestSuggestName(t *testing.T) {
	taken := map[string]bool{}
	isTaken := func(n string) bool { return taken[n] }
	if n := suggestName("kasl", "db", validDBName, isTaken); n != "kasl-db" {
		t.Errorf("first = %q", n)
	}
	taken["kasl-db"], taken["kasl-db-2"] = true, true
	if n := suggestName("kasl", "db", validDBName, isTaken); n != "kasl-db-3" {
		t.Errorf("after two = %q", n)
	}
	long := "a-very-long-project-name-that-fills-up-"
	n := suggestName(long+"x", "storage", validName, isTaken)
	if !validName.MatchString(n) || !strings.HasSuffix(n, "-storage") || strings.Contains(n, "--") {
		t.Errorf("long project = %q", n)
	}
}

func TestBucketName(t *testing.T) {
	for _, c := range []struct{ project, storage, want string }{
		{"kasl", "kasl-storage", "kasl-storage-a1b2c3"},
		{"kasl", "media", "kasl-media-a1b2c3"},
		{"kasl", "kaslmedia", "kasl-kaslmedia-a1b2c3"},
	} {
		if got := bucketName(c.project, c.storage, "a1b2c3"); got != c.want {
			t.Errorf("bucketName(%q, %q) = %q", c.project, c.storage, got)
		}
	}
	if got := bucketName(strings.Repeat("p", 40), strings.Repeat("s", 40), "a1b2c3"); len(got) > 63 || !strings.HasSuffix(got, "-a1b2c3") {
		t.Errorf("long names = %q (%d)", got, len(got))
	}
}
