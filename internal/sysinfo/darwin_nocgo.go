//go:build darwin && !cgo

package sysinfo

// Without cgo, macOS offers no per-process counters or host statistics to pure Go; the
// process table, memory, priority, pausing and disk space still work.

// PerProcess reports whether ProcUsage measures on this build.
const PerProcess = false

func ProcUsage(int) (Usage, bool) { return Usage{}, false }

func MachineStats() (Machine, error) {
	return Machine{Cores: ncpu(), MemTotal: memTotal(), MemFree: memFree(), CommitRAMBased: true}, ErrUnsupported
}

func Disks() ([]Disk, error) { return nil, ErrUnsupported }
