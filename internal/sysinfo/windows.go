//go:build windows

package sysinfo

import (
	"runtime"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PerProcess reports whether ProcUsage measures on this build.
const PerProcess = true

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	ntdll                    = windows.NewLazySystemDLL("ntdll.dll")
	procGetProcessIoCounters = kernel32.NewProc("GetProcessIoCounters")
	procGetProcessMemoryInfo = kernel32.NewProc("K32GetProcessMemoryInfo")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procNtSuspendProcess     = ntdll.NewProc("NtSuspendProcess")
	procNtResumeProcess      = ntdll.NewProc("NtResumeProcess")
)

const (
	belowNormalPriorityClass = 0x00004000
	processSuspendResume     = 0x0800
	stillActive              = 259
)

type ioCounters struct {
	ReadOps, WriteOps, OtherOps, ReadBytes, WriteBytes, OtherBytes uint64
}

type memCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type memStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func filetime(ft windows.Filetime) time.Duration {
	return time.Duration(uint64(ft.HighDateTime)<<32|uint64(ft.LowDateTime)) * 100
}

// Procs lists every process (one toolhelp snapshot). Windows has no process groups and the
// each readable process includes its creation token and image name. Unknowns fail closed.
func Procs() ([]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var out []Proc
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID != 0 {
			p := Proc{PID: int(e.ProcessID), PPID: int(e.ParentProcessID), Name: windows.UTF16ToString(e.ExeFile[:])}
			h, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, e.ProcessID)
			if openErr == nil {
				p.Start = startTime(h)
				windows.CloseHandle(h)
			}
			out = append(out, p)
		}
	}
	return out, nil
}

func startTime(h windows.Handle) int64 {
	var c, x, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &x, &k, &u) != nil {
		return 0
	}
	return int64(uint64(c.HighDateTime)<<32 | uint64(c.LowDateTime)) // raw 100ns ticks; nanoseconds overflow
}

// ProcUsage reads CPU times, working set and I/O transfer counts.
func ProcUsage(pid int) (Usage, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return Usage{}, false
	}
	defer windows.CloseHandle(h)
	var c, x, k, us windows.Filetime
	if windows.GetProcessTimes(h, &c, &x, &k, &us) != nil {
		return Usage{}, false
	}
	u := Usage{CPU: filetime(k) + filetime(us)}
	var mc memCounters
	mc.Cb = uint32(unsafe.Sizeof(mc))
	if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.Cb)); r != 0 {
		u.Mem = uint64(mc.WorkingSetSize)
	}
	var io ioCounters
	if r, _, _ := procGetProcessIoCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&io))); r != 0 {
		u.Written, u.Read = io.WriteBytes, io.ReadBytes
	}
	return u, true
}

// MachineStats reads GetSystemTimes and GlobalMemoryStatusEx.
func MachineStats() (Machine, error) {
	m := Machine{Cores: runtime.NumCPU()}
	var idle, kern, user windows.Filetime
	if r, _, e := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kern)), uintptr(unsafe.Pointer(&user))); r == 0 {
		return m, e
	}
	m.CPUTotal = filetime(kern) + filetime(user) // kernel time includes idle
	m.CPUBusy = m.CPUTotal - filetime(idle)
	var ms memStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	if r, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r == 0 {
		return m, e
	}
	m.MemTotal, m.MemFree = ms.TotalPhys, ms.AvailPhys
	m.CommitFree, m.CommitKnown = ms.AvailPageFile, true // CommitLimit - CommitTotal
	if v, ok := pagesInPerSec(); ok {
		m.Pressure = Pressure{Kind: PressurePagesIn, Value: v}
	}
	return m, nil
}

// diskPerformance is DISK_PERFORMANCE (winioctl.h); times are 100 ns units.
type diskPerformance struct {
	BytesRead, BytesWritten, ReadTime, WriteTime, IdleTime int64
	ReadCount, WriteCount, QueueDepth, SplitCount          uint32
	QueryTime                                              int64
	StorageDeviceNumber                                    uint32
	StorageManagerName                                     [8]uint16
}

const (
	ioctlDiskPerformance = 0x70020 // IOCTL_DISK_PERFORMANCE
	maxPhysicalDrives    = 32
)

// Disks asks every \\.\PhysicalDriveN for its IOCTL_DISK_PERFORMANCE counters. The drive
// is opened with zero access rights (query only, no admin needed). ReadTime+WriteTime is
// the summed duration of every completed request, the same in-flight integral as Linux's
// weighted time ("Avg. Disk Queue Length" is built from it). Drives that answer nothing
// (counters off, removable, absent) are left out; none at all is ErrUnsupported.
func Disks() ([]Disk, error) {
	var out []Disk
	for i := 0; i < maxPhysicalDrives; i++ {
		name := "PhysicalDrive" + strconv.Itoa(i)
		path, _ := windows.UTF16PtrFromString(`\\.\` + name)
		h, err := windows.CreateFile(path, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			continue
		}
		var dp diskPerformance
		var n uint32
		err = windows.DeviceIoControl(h, ioctlDiskPerformance, nil, 0, (*byte)(unsafe.Pointer(&dp)), uint32(unsafe.Sizeof(dp)), &n, nil)
		windows.CloseHandle(h)
		if err != nil || n < uint32(unsafe.Offsetof(dp.QueryTime)) || dp.ReadTime < 0 || dp.WriteTime < 0 || dp.BytesWritten < 0 {
			continue
		}
		out = append(out, Disk{Name: name, IOTime: time.Duration(dp.ReadTime+dp.WriteTime) * 100, Written: uint64(dp.BytesWritten)})
	}
	if len(out) == 0 {
		return nil, ErrUnsupported
	}
	return out, nil
}

// LowerPriority puts pid in BELOW_NORMAL_PRIORITY_CLASS; processes it starts afterwards
// inherit it.
func LowerPriority(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_INFORMATION, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.SetPriorityClass(h, belowNormalPriorityClass)
}

// Alive reports whether a process with this PID is running.
func Alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == stillActive
}

// DiskSpace is the free (for this user) and total bytes of the volume holding path.
func DiskSpace(path string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var all uint64
	err = windows.GetDiskFreeSpaceEx(p, &free, &total, &all)
	return free, total, err
}

// SelfCPU is the CPU time this process used so far.
func SelfCPU() time.Duration {
	var c, x, k, u windows.Filetime
	if windows.GetProcessTimes(windows.CurrentProcess(), &c, &x, &k, &u) != nil {
		return 0
	}
	return filetime(k) + filetime(u)
}
