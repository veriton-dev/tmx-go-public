//go:build linux

package sysinfo

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// PerProcess reports whether ProcUsage measures on this build.
const PerProcess = true

// clockTick is USER_HZ, 100 on every mainstream Linux.
const clockTick = 10 * time.Millisecond

var pageSize = uint64(os.Getpagesize())

// statFields returns the fields of /proc/<pid>/stat after the command name (field 3 = [0]).
func statFields(pid string) ([]string, bool) {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return nil, false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 > len(b) {
		return nil, false
	}
	return strings.Fields(string(b[i+2:])), true
}

// Procs walks /proc (one small read per process).
func Procs() ([]Proc, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]Proc, 0, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		f, ok := statFields(e.Name())
		if !ok || len(f) < 20 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		pgid, _ := strconv.Atoi(f[2])
		start, _ := strconv.ParseInt(f[19], 10, 64)
		name, _ := os.ReadFile("/proc/" + e.Name() + "/comm")
		out = append(out, Proc{PID: pid, PPID: ppid, PGID: pgid, Start: start, Name: strings.TrimSpace(string(name))})
	}
	return out, nil
}

// ProcUsage reads /proc/<pid>/stat (CPU, resident pages) and /proc/<pid>/io (bytes that
// reached the storage layer; readable for the user's own processes).
func ProcUsage(pid int) (Usage, bool) {
	s := strconv.Itoa(pid)
	f, ok := statFields(s)
	if !ok || len(f) < 22 {
		return Usage{}, false
	}
	ut, _ := strconv.ParseUint(f[11], 10, 64)
	st, _ := strconv.ParseUint(f[12], 10, 64)
	rss, _ := strconv.ParseUint(f[21], 10, 64)
	u := Usage{CPU: time.Duration(ut+st) * clockTick, Mem: rss * pageSize}
	if b, err := os.ReadFile("/proc/" + s + "/io"); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			switch k {
			case "write_bytes":
				u.Written = n
			case "read_bytes":
				u.Read = n
			}
		}
	}
	return u, true
}

// MachineStats reads /proc/stat and /proc/meminfo.
func MachineStats() (Machine, error) {
	m := Machine{Cores: runtime.NumCPU(), CommitRAMBased: true}
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return m, err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return m, errors.New("/proc/stat: no cpu line")
	}
	var total, idle uint64
	for i, v := range f[1:] {
		if i >= 8 { // guest time is already in user
			break
		}
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	m.CPUTotal = time.Duration(total) * clockTick
	m.CPUBusy = time.Duration(total-idle) * clockTick
	b, err = os.ReadFile("/proc/meminfo")
	if err != nil {
		return m, err
	}
	var swapFree uint64
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		switch k {
		case "MemTotal":
			m.MemTotal = n << 10
		case "MemAvailable":
			m.MemFree = n << 10
			m.CommitKnown = true
		case "SwapFree":
			swapFree = n << 10
		}
	}
	m.CommitFree = m.MemFree + swapFree
	if b, err := os.ReadFile("/proc/pressure/memory"); err == nil { // absent on old kernels: no signal
		if v, ok := parsePSI(string(b)); ok {
			m.Pressure = Pressure{Kind: PressurePSI, Value: v}
		}
	}
	return m, nil
}

// parsePSI returns the "some avg10" figure of /proc/pressure/memory.
func parsePSI(text string) (float64, bool) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, part := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(part, "avg10="); ok {
				f, err := strconv.ParseFloat(v, 64)
				return f, err == nil
			}
		}
	}
	return 0, false
}

// Disks reads /proc/diskstats for whole disks (/sys/block entries, no partitions or
// virtual devices).
func Disks() ([]Disk, error) {
	b, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return nil, err
	}
	var out []Disk
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 14 {
			continue
		}
		name := f[2]
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") ||
			strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "sr") || strings.HasPrefix(name, "md") {
			continue
		}
		if _, err := os.Stat("/sys/block/" + name); err != nil {
			continue
		}
		sectors, _ := strconv.ParseUint(f[9], 10, 64)
		weighted, _ := strconv.ParseUint(f[13], 10, 64) // ms, time-in-queue integral
		out = append(out, Disk{Name: name, IOTime: time.Duration(weighted) * time.Millisecond, Written: sectors * 512})
	}
	return out, nil
}

// Getpriority's raw syscall answers 20-nice.
func currentNice(prio int) int { return 20 - prio }

// lowerIO puts pid in the lowest best-effort I/O class level (inherited by children).
func lowerIO(pid int) error {
	const whoProcess, classBE, level = 1, 2, 7
	_, _, e := unix.Syscall(unix.SYS_IOPRIO_SET, whoProcess, uintptr(pid), classBE<<13|level)
	if e != 0 {
		return e
	}
	return nil
}
