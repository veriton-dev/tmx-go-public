//go:build windows

package sysinfo

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	pdh                         = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQuery            = pdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounter    = pdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData     = pdh.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterV = pdh.NewProc("PdhGetFormattedCounterValue")
)

const pdhFmtDouble = 0x200

// pdhValue is PDH_FMT_COUNTERVALUE for a double.
type pdhValue struct {
	Status uint32
	_      uint32
	Value  float64
}

var pagesIn struct {
	sync.Mutex
	query, counter uintptr
	tried, ok      bool
}

// pagesInPerSec is \Memory\Pages Input/sec (hard page faults). The first call only opens
// the query; a failure of any step means "no signal", never evidence of trouble.
func pagesInPerSec() (float64, bool) {
	p := &pagesIn
	p.Lock()
	defer p.Unlock()
	if !p.tried {
		p.tried = true
		if r, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&p.query))); r != 0 {
			return 0, false
		}
		path, _ := windows.UTF16PtrFromString(`\Memory\Pages Input/sec`)
		if r, _, _ := procPdhAddEnglishCounter.Call(p.query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&p.counter))); r != 0 {
			return 0, false
		}
		procPdhCollectQueryData.Call(p.query)
		p.ok = true
		return 0, false // a rate needs two collections
	}
	if !p.ok {
		return 0, false
	}
	if r, _, _ := procPdhCollectQueryData.Call(p.query); r != 0 {
		return 0, false
	}
	var v pdhValue
	if r, _, _ := procPdhGetFormattedCounterV.Call(p.counter, pdhFmtDouble, 0, uintptr(unsafe.Pointer(&v))); r != 0 || v.Status != 0 {
		return 0, false
	}
	return v.Value, true
}
