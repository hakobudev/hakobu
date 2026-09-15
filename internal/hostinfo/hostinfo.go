// Package hostinfo reads basic OS-level metrics (load, memory, disk) straight
// from /proc and syscall.Statfs — hako only ever runs on Linux hosts,
// so there's no need for a cross-platform metrics library for three numbers.
package hostinfo

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"sync"
	"strings"
	"syscall"
	"time"
)

type Host struct {
	CPUPercent              float64
	Load1, Load5, Load15    float64
	MemTotalMB, MemUsedMB   float64
	DiskTotalGB, DiskUsedGB float64
}

func Get(diskPath string) (Host, error) {
	var h Host

	if cpu, err := CPUPercent(); err == nil {
		h.CPUPercent = cpu
	}

	load, err := readLoadAvg()
	if err != nil {
		return h, err
	}
	h.Load1, h.Load5, h.Load15 = load[0], load[1], load[2]

	memTotal, memAvail, err := readMemInfo()
	if err != nil {
		return h, err
	}
	const kb = 1024.0
	h.MemTotalMB = memTotal / kb
	h.MemUsedMB = (memTotal - memAvail) / kb

	var stat syscall.Statfs_t
	if err := syscall.Statfs(diskPath, &stat); err != nil {
		return h, err
	}
	const gb = 1024.0 * 1024.0 * 1024.0
	total := float64(stat.Blocks) * float64(stat.Bsize)
	free := float64(stat.Bavail) * float64(stat.Bsize)
	h.DiskTotalGB = total / gb
	h.DiskUsedGB = (total - free) / gb

	return h, nil
}

// CPUPercent takes two /proc/stat samples ~150ms apart and returns overall
// host CPU usage — the same delta technique docker stats uses for
// containers, just against the whole machine's jiffy counters.
func CPUPercent() (float64, error) {
	idle1, total1, err := readCPUStat()
	if err != nil {
		return 0, err
	}
	time.Sleep(150 * time.Millisecond)
	idle2, total2, err := readCPUStat()
	if err != nil {
		return 0, err
	}

	totalDelta := total2 - total1
	idleDelta := idle2 - idle1
	if totalDelta <= 0 {
		return 0, nil
	}
	return (1 - float64(idleDelta)/float64(totalDelta)) * 100, nil
}

func readCPUStat() (idle, total uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, 0, fmt.Errorf("empty /proc/stat")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("unexpected /proc/stat format")
	}

	var values []uint64
	for _, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			break
		}
		values = append(values, v)
		total += v
	}
	if len(values) >= 4 {
		idle = values[3] // idle
		if len(values) >= 5 {
			idle += values[4] // + iowait
		}
	}
	return idle, total, nil
}

var (
	lastSelfCPU   uint64
	lastSelfCPUAt time.Time
	selfCPUMu     sync.Mutex
)

// SelfCPUPercent reports the calling process's own CPU usage since the
// previous call, using /proc/self/stat jiffy counters — cheap and needs no
// blocking sample pair since we already have a previous reading from the
// last poller tick.
func SelfCPUPercent() (float64, error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, err
	}
	// Fields after the ")" that closes the comm field are space-separated;
	// utime is field 14, stime field 15 (1-indexed) of the whole line.
	closeParen := strings.LastIndex(string(data), ")")
	if closeParen == -1 {
		return 0, fmt.Errorf("unexpected /proc/self/stat format")
	}
	fields := strings.Fields(string(data)[closeParen+1:])
	if len(fields) < 14 {
		return 0, fmt.Errorf("unexpected /proc/self/stat format")
	}
	utime, _ := strconv.ParseUint(fields[11], 10, 64)
	stime, _ := strconv.ParseUint(fields[12], 10, 64)
	total := utime + stime

	selfCPUMu.Lock()
	defer selfCPUMu.Unlock()
	now := time.Now()
	defer func() { lastSelfCPU, lastSelfCPUAt = total, now }()

	if lastSelfCPUAt.IsZero() {
		return 0, nil
	}
	const clockTicksPerSec = 100.0
	elapsed := now.Sub(lastSelfCPUAt).Seconds()
	if elapsed <= 0 {
		return 0, nil
	}
	deltaTicks := float64(total - lastSelfCPU)
	return (deltaTicks / clockTicksPerSec) / elapsed * 100, nil
}

func readLoadAvg() ([3]float64, error) {
	var out [3]float64
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return out, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return out, fmt.Errorf("unexpected /proc/loadavg format")
	}
	for i := 0; i < 3; i++ {
		out[i], err = strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func readMemInfo() (totalKB, availableKB float64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, _ := strconv.ParseFloat(fields[1], 64)
		switch fields[0] {
		case "MemTotal:":
			totalKB = value
		case "MemAvailable:":
			availableKB = value
		}
	}
	return totalKB, availableKB, nil
}
