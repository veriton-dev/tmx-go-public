//go:build darwin && cgo

package sysinfo

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <libproc.h>
#include <sys/resource.h>
#include <mach/mach.h>
#include <mach/mach_time.h>
#include <IOKit/IOKitLib.h>
#include <IOKit/storage/IOBlockStorageDriver.h>
#include <CoreFoundation/CoreFoundation.h>

static int tmx_rusage(int pid, uint64_t *cpu, uint64_t *mem, uint64_t *wr, uint64_t *rd) {
	struct rusage_info_v2 ri;
	if (proc_pid_rusage(pid, RUSAGE_INFO_V2, (rusage_info_t *)&ri) != 0) return -1;
	*cpu = ri.ri_user_time + ri.ri_system_time; // mach absolute time units
	*mem = ri.ri_phys_footprint;
	*wr = ri.ri_diskio_byteswritten;
	*rd = ri.ri_diskio_bytesread;
	return 0;
}

static void tmx_timebase(uint32_t *numer, uint32_t *denom) {
	mach_timebase_info_data_t tb;
	mach_timebase_info(&tb);
	*numer = tb.numer;
	*denom = tb.denom;
}

static mach_port_t tmx_host;

static int tmx_machine(uint64_t *ticks) {
	if (!tmx_host) tmx_host = mach_host_self();
	host_cpu_load_info_data_t cpu;
	mach_msg_type_number_t n = HOST_CPU_LOAD_INFO_COUNT;
	if (host_statistics(tmx_host, HOST_CPU_LOAD_INFO, (host_info_t)&cpu, &n) != KERN_SUCCESS) return -1;
	for (int i = 0; i < CPU_STATE_MAX; i++) ticks[i] = cpu.cpu_ticks[i];
	return 0;
}

static int64_t tmx_num(CFDictionaryRef d, CFStringRef key) {
	int64_t v = 0;
	CFNumberRef n = (CFNumberRef)CFDictionaryGetValue(d, key);
	if (n && CFGetTypeID(n) == CFNumberGetTypeID()) CFNumberGetValue(n, kCFNumberSInt64Type, &v);
	return v;
}

// tmx_disks fills (io time ns, bytes written) per IOBlockStorageDriver; returns the count.
static int tmx_disks(int64_t *out, int max) {
	io_iterator_t it;
	if (IOServiceGetMatchingServices(0, IOServiceMatching("IOBlockStorageDriver"), &it) != KERN_SUCCESS) return -1;
	int n = 0;
	io_registry_entry_t e;
	while ((e = IOIteratorNext(it)) != 0) {
		CFMutableDictionaryRef props = NULL;
		if (n < max && IORegistryEntryCreateCFProperties(e, &props, kCFAllocatorDefault, 0) == KERN_SUCCESS && props) {
			CFDictionaryRef st = (CFDictionaryRef)CFDictionaryGetValue(props, CFSTR(kIOBlockStorageDriverStatisticsKey));
			if (st && CFGetTypeID(st) == CFDictionaryGetTypeID()) {
				out[2*n] = tmx_num(st, CFSTR(kIOBlockStorageDriverStatisticsTotalReadTimeKey)) +
					tmx_num(st, CFSTR(kIOBlockStorageDriverStatisticsTotalWriteTimeKey));
				out[2*n+1] = tmx_num(st, CFSTR(kIOBlockStorageDriverStatisticsBytesWrittenKey));
				n++;
			}
			CFRelease(props);
		}
		IOObjectRelease(e);
	}
	IOObjectRelease(it);
	return n;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// PerProcess reports whether ProcUsage measures on this build.
const PerProcess = true

var (
	tbOnce       sync.Once
	tbNum, tbDen uint64
)

func machToDuration(t uint64) time.Duration {
	tbOnce.Do(func() {
		var n, d C.uint32_t
		C.tmx_timebase(&n, &d)
		tbNum, tbDen = uint64(n), uint64(d)
		if tbNum == 0 || tbDen == 0 {
			tbNum, tbDen = 1, 1
		}
	})
	return time.Duration(t / tbDen * tbNum)
}

// ProcUsage reads one process's counters (proc_pid_rusage; the user's own processes only).
func ProcUsage(pid int) (Usage, bool) {
	var cpu, mem, wr, rd C.uint64_t
	if C.tmx_rusage(C.int(pid), &cpu, &mem, &wr, &rd) != 0 {
		return Usage{}, false
	}
	return Usage{CPU: machToDuration(uint64(cpu)), Mem: uint64(mem), Written: uint64(wr), Read: uint64(rd)}, true
}

// MachineStats reads CPU ticks (host_statistics) and memory (sysctl).
func MachineStats() (Machine, error) {
	var ticks [C.CPU_STATE_MAX]C.uint64_t
	if C.tmx_machine(&ticks[0]) != 0 {
		return Machine{}, errors.New("host_statistics failed")
	}
	// Ticks are 1/100 s (CLK_TCK) summed over cores.
	const tick = 10 * time.Millisecond
	idle := time.Duration(ticks[C.CPU_STATE_IDLE]) * tick
	var total time.Duration
	for _, t := range ticks {
		total += time.Duration(t) * tick
	}
	return Machine{CPUBusy: total - idle, CPUTotal: total, Cores: ncpu(),
		MemTotal: memTotal(), MemFree: memFree(), CommitRAMBased: true}, nil
}

// Disks reads every block storage driver's statistics (IOKit, no root needed).
func Disks() ([]Disk, error) {
	var buf [64]C.int64_t
	n := int(C.tmx_disks(&buf[0], C.int(len(buf)/2)))
	if n < 0 {
		return nil, errors.New("IOKit: no block storage statistics")
	}
	out := make([]Disk, n)
	for i := range out {
		out[i] = Disk{Name: fmt.Sprintf("disk%d", i), IOTime: time.Duration(buf[2*i]), Written: uint64(buf[2*i+1])}
	}
	return out, nil
}
