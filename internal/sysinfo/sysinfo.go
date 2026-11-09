// Package sysinfo measures processes and the machine cheaply, and lowers, pauses and resumes
// agent process trees. Everything here is per OS (darwin, linux, windows); what an OS or a
// build cannot measure reports ok=false or ErrUnsupported, never a made-up number.
//
// Safety: SuspendTree acts only on a tree whose root is a direct child of this process
// (an agent the daemon started), with verified creation times and parent chains.
package sysinfo

import (
	"errors"
	"time"
)

// ErrUnsupported: this OS or build cannot do it.
var ErrUnsupported = errors.New("not supported on this system")

// ErrNotChild: SuspendTree was asked to act on a process this daemon did not start.
var ErrNotChild = errors.New("the process is not a child of this daemon")

// Proc is one row of the process table.
type Proc struct {
	PID   int    `json:"pid"`
	PPID  int    `json:"ppid"`
	PGID  int    `json:"pgid"`           // 0 where the OS has no process groups (Windows)
	Name  string `json:"name,omitempty"` // executable basename; unknown names fail closed for control
	Start int64  `json:"start"`          // start-time token (OS-specific units); tells a reused PID apart; 0 = unknown
}

// Usage is what one process used so far (cumulative counters).
type Usage struct {
	CPU     time.Duration // user + system time
	Mem     uint64        // resident memory in bytes (macOS: physical footprint)
	Written uint64        // bytes written to storage
	Read    uint64        // bytes read from storage
}

// Machine is the machine's state: cumulative CPU time counters and current memory.
type Machine struct {
	CPUBusy  time.Duration // all cores, cumulative; CPUTotal likewise (busy + idle)
	CPUTotal time.Duration
	Cores    int
	MemTotal uint64
	MemFree  uint64 // available for new work without swapping (OS estimate)
	// CommitFree is the headroom before allocations fail: Windows CommitLimit-CommitTotal,
	// Linux MemAvailable+SwapFree. CommitKnown false = this build cannot tell (macOS).
	CommitFree  uint64
	CommitKnown bool
	// CommitRAMBased marks RAM-plus-swap headroom, whose default guard floors
	// scale with physical RAM. False preserves absolute commit-limit floors.
	// The reader supplies this semantic so injected readings are host-independent.
	CommitRAMBased bool
	// Pressure is the OS's paging signal; it only gates the stop level (with RAM low).
	Pressure Pressure
}

// Pressure kinds.
const (
	PressurePagesIn = "pages_in" // Windows: hard page faults per second (Memory\Pages Input/sec)
	PressurePSI     = "psi"      // Linux: /proc/pressure/memory "some avg10", percent
)

// Pressure is one reading of the OS's paging signal; Kind "" = not available here.
type Pressure struct {
	Kind  string
	Value float64
}

// Disk is one physical disk's cumulative counters.
type Disk struct {
	Name    string
	IOTime  time.Duration // integral of requests in flight over time (Δ/Δwall = average queue depth)
	Written uint64        // bytes written
}

// Tree returns only descendants with a proven parent chain and creation-time ordering.
// Group membership alone is not proof of ownership; unknown timestamps fail closed.
// The root comes first. Pure; procs is a snapshot from Procs.
func Tree(procs []Proc, root int) []Proc {
	children := make(map[int][]int, len(procs))
	byPID := make(map[int]Proc, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	rp, ok := byPID[root]
	if !ok || rp.Start <= 0 {
		return nil
	}
	in := map[int]bool{root: true}
	out := []Proc{rp}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if c != pid && !in[c] && byPID[c].Start >= byPID[pid].Start && byPID[c].Start > 0 {
				in[c] = true
				out = append(out, byPID[c])
				queue = append(queue, c)
			}
		}
	}
	return out
}

func find(procs []Proc, pid int) (Proc, bool) {
	for _, p := range procs {
		if p.PID == pid {
			return p, true
		}
	}
	return Proc{}, false
}

// LowPriorityCommand returns the command line that starts path with args at low I/O
// priority, where the OS needs a wrapper for that: on macOS /usr/sbin/taskpolicy -d utility,
// which sets the disk I/O policy and execs the program (same PID; children inherit it).
// Elsewhere it returns path and args unchanged (Linux sets the I/O class after the start,
// Windows' below-normal class covers I/O). CPU priority is LowerPriority's job.
func LowPriorityCommand(path string, args []string) (string, []string) {
	return wrapLow(path, args)
}
