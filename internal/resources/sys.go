package resources

import (
	"os"

	"github.com/blackcode-ch/tmx-go/internal/sysinfo"
)

// Sys is the measuring layer: sysinfo on this machine, a fake in tests.
type Sys interface {
	Procs() ([]sysinfo.Proc, error)
	ProcUsage(pid int) (sysinfo.Usage, bool)
	Machine() (sysinfo.Machine, error)
	Disks() ([]sysinfo.Disk, error)
	DiskSpace(path string) (free, total uint64, err error)
	ResumeProcs([]sysinfo.Proc) int
}

type osSys struct{}

func (osSys) Procs() ([]sysinfo.Proc, error)             { return sysinfo.Procs() }
func (osSys) ProcUsage(pid int) (sysinfo.Usage, bool)    { return sysinfo.ProcUsage(pid) }
func (osSys) Machine() (sysinfo.Machine, error)          { return sysinfo.MachineStats() }
func (osSys) Disks() ([]sysinfo.Disk, error)             { return sysinfo.Disks() }
func (osSys) DiskSpace(p string) (uint64, uint64, error) { return sysinfo.DiskSpace(p) }
func (osSys) ResumeProcs(l []sysinfo.Proc) int           { return sysinfo.ResumeProcs(l) }

func tempDir() string { return os.TempDir() }
