package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/panellog"
)

// The disk reserve is a file hakobu keeps on the disk Docker uses, and
// deletes when that disk is full: then Postgres has room to VACUUM, logs
// and hakobu's own databases to write, while the cause is cleaned up. On
// kasl in 2026-10 only ext4's 5% kept for root made that possible, and
// only from a root shell.

// ReserveFile is the reserve, next to hakobu's data.
const ReserveFile = "data/disk-reserve"

const (
	maxReserve = 1 << 30
	// reserveGone: free space under this (or the disk 98% full) gives the
	// reserve back; reserveBack: it's made again only with this many
	// times its size free and the disk under 85%.
	reserveGone = 256 << 20
	reserveBack = 4
)

// reserveSize is the reserve on a disk of total bytes: 1 GB, or 2% of a
// small disk.
func reserveSize(total uint64) int64 {
	return min(maxReserve, int64(total/50))
}

type reserveAction int

const (
	reserveKeep reserveAction = iota
	reserveMake
	reserveFree
)

// reserveStep is what to do with the reserve: free bytes are left of
// total, and it's there or not.
func reserveStep(free, total uint64, there bool) reserveAction {
	if total == 0 {
		return reserveKeep
	}
	size := uint64(reserveSize(total))
	used := float64(total-free) / float64(total)
	switch {
	case there && (free < reserveGone || used >= 0.98):
		return reserveFree
	case !there && free > reserveBack*size && used < 0.85:
		return reserveMake
	}
	return reserveKeep
}

// KeepReserve keeps the reserve at path every minute until ctx ends,
// if path is on Docker's disk: a reserve elsewhere frees nothing there.
func KeepReserve(ctx context.Context, path string) {
	warned := false
	for {
		if err := keepReserve(ctx, path); err != nil && !warned {
			panellog.Warn("disk reserve:", err)
			warned = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

var errOtherDisk = errors.New("hakobu's data isn't on Docker's disk; keeping none")

func keepReserve(ctx context.Context, path string) error {
	root, err := deploy.DockerRootDir(ctx)
	if err != nil {
		return err
	}
	var docker, here syscall.Statfs_t
	if err := syscall.Statfs(root, &docker); err != nil {
		return err
	}
	if err := syscall.Statfs(filepath.Dir(path), &here); err != nil {
		return err
	}
	if !sameDisk(docker, here) {
		_ = os.Remove(path)
		return errOtherDisk
	}
	bs := uint64(docker.Bsize)
	free, total := docker.Bavail*bs, docker.Blocks*bs
	_, err = os.Stat(path)
	there := err == nil
	switch reserveStep(free, total, there) {
	case reserveFree:
		if err := os.Remove(path); err != nil {
			return err
		}
		panellog.Errorf("disk full: gave back hakobu's %d MB reserve, so databases and logs can write while you free space", reserveSize(total)>>20)
	case reserveMake:
		return makeReserve(path, reserveSize(total))
	}
	return nil
}

// sameDisk reports whether two statfs readings are of one disk: the same
// filesystem, or subvolumes of one pool (btrfs gives each its own fsid),
// which have its size and, read together, its free space, give or take
// what was written between the readings.
func sameDisk(a, b syscall.Statfs_t) bool {
	if a.Fsid == b.Fsid {
		return true
	}
	diff := int64(a.Bfree) - int64(b.Bfree)
	return a.Bsize == b.Bsize && a.Blocks == b.Blocks && max(diff, -diff) <= int64(a.Blocks/100)
}

// makeReserve allocates size bytes at path: written out, not sparse, so
// they're really taken.
func makeReserve(path string, size int64) error {
	f, err := os.OpenFile(path+".new", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, size); err != nil {
		f.Close()
		os.Remove(path + ".new")
		return fmt.Errorf("allocating %d MB: %w", size>>20, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path + ".new")
		return err
	}
	return os.Rename(path+".new", path)
}
