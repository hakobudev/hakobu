package node

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The reserve goes when the disk is full, and comes back only once there's
// plenty of room, so it doesn't flap.
func TestReserveStep(t *testing.T) {
	const gb = 1 << 30
	for _, c := range []struct {
		free, total uint64
		there       bool
		want        reserveAction
	}{
		{20 * gb, 40 * gb, true, reserveKeep},
		{20 * gb, 40 * gb, false, reserveMake},
		{100 << 20, 40 * gb, true, reserveFree},  // under 256 MB free
		{700 << 20, 40 * gb, true, reserveFree},  // 98% full
		{2 * gb, 40 * gb, false, reserveKeep},    // freed, not yet room to make it again
		{5 * gb, 40 * gb, false, reserveKeep},    // 87%: still too full
		{3 * gb, 10 * gb, false, reserveMake},    // 200 MB on a small disk
		{100 << 20, 40 * gb, false, reserveKeep}, // gone already
		{0, 0, true, reserveKeep},                // no reading
	} {
		if got := reserveStep(c.free, c.total, c.there); got != c.want {
			t.Errorf("%d MB free of %d GB, there %v: %d, want %d", c.free>>20, c.total/gb, c.there, got, c.want)
		}
	}
	if got := reserveSize(10 * gb); got != 10*gb/50 {
		t.Errorf("reserve of a 10 GB disk: %d", got)
	}
}

// The reserve's bytes are allocated, not a sparse file.
func TestMakeReserve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk-reserve")
	if err := makeReserve(path, 8<<20); err != nil {
		t.Skip("no fallocate here:", err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() != 8<<20 {
		t.Fatalf("%v %v", fi, err)
	}
	if st, ok := fi.Sys().(interface{ Blocks() int64 }); ok && st.Blocks()*512 < 8<<20 {
		t.Error("sparse")
	}
}

// btrfs subvolumes are one disk though their fsids differ; two disks of
// one size aren't.
func TestSameDisk(t *testing.T) {
	root := syscall.Statfs_t{Bsize: 4096, Blocks: 62098688, Bfree: 30000000}
	root.Fsid.X__val = [2]int32{1, 2}
	home := root
	home.Fsid.X__val = [2]int32{1, 3}
	home.Bfree -= 100 // written in between
	other := root
	other.Fsid.X__val = [2]int32{9, 9}
	other.Bfree = 50000000
	if !sameDisk(root, root) || !sameDisk(root, home) || sameDisk(root, other) {
		t.Error("wrong")
	}
}
