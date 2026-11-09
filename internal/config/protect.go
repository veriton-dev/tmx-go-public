package config

import (
	"fmt"
	"slices"
	"strings"
)

// MemGuard is resources.memguard: the machine's memory guard (parity with Python tmx's
// memguard block, same key names and defaults). Levels, each an OR of its two thresholds:
//
//	warn    free commit < commit_warn_gb   or available RAM < ram_warn_pct    (alert only)
//	refuse  free commit < commit_refuse_gb or available RAM < ram_refuse_pct  (no new agent starts)
//	stop    free commit < commit_stop_gb   or available RAM < ram_stop_pct    (agents are paused
//	        or sustained paging with RAM below 20%                              one by one, never killed)
//	orphan  free commit < commit_orphan_gb or available RAM < ram_orphan_pct  (acts as stop)
//
// Zero fields take the default; a negative threshold switches that threshold off.
type MemGuard struct {
	Enabled *bool `json:"enabled,omitempty"` // nil = on

	CommitWarnGB   float64 `json:"commit_warn_gb"`
	RAMWarnPct     float64 `json:"ram_warn_pct"`
	CommitRefuseGB float64 `json:"commit_refuse_gb"`
	RAMRefusePct   float64 `json:"ram_refuse_pct"`
	CommitStopGB   float64 `json:"commit_stop_gb"`
	RAMStopPct     float64 `json:"ram_stop_pct"`
	CommitOrphanGB float64 `json:"commit_orphan_gb"`
	RAMOrphanPct   float64 `json:"ram_orphan_pct"`

	// Sustained paging only counts together with RAM already below 20%.
	PagingPagesInS float64 `json:"paging_pages_in_s"` // Windows hard faults per second
	PagingSustainS float64 `json:"paging_sustain_s"`  // held this long
	PSIAvg10Pct    float64 `json:"psi_avg10_pct"`     // Linux "some avg10"

	HysteresisS  float64 `json:"hysteresis_s"`   // a stop episode ends at "ok" or after this long
	RestopAfterS float64 `json:"restop_after_s"` // re-measure this long after each pause

	// The warn level clears only once RAM is warn+WarnClearPctMargin and free commit is
	// warn+WarnClearGBMargin, both held for WarnClearHoldS.
	WarnClearPctMargin float64 `json:"warn_clear_pct_margin"`
	WarnClearGBMargin  float64 `json:"warn_clear_gb_margin"`
	WarnClearHoldS     float64 `json:"warn_clear_hold_s"`

	// CriticalAgents are never paused by name, on top of the head (always critical).
	CriticalAgents []string `json:"critical_agents,omitempty"`
}

// On reports whether the guard runs.
func (g MemGuard) On() bool { return g.Enabled == nil || *g.Enabled }

// DiskReserve is resources.disk_reserve: every watched volume keeps GB free (or Pct of the
// volume, whichever is more). Below it the agent writing most to that volume is paused
// (never killed; resume with `tmx resume`), no new agent starts and no new copy is packed.
type DiskReserve struct {
	Mode       string  `json:"mode"`        // on (default) | observe (alerts only) | off
	GB         float64 `json:"gb"`          // 5 by default; negative = 0
	Pct        float64 `json:"pct"`         // 0 by default
	WarnFactor float64 `json:"warn_factor"` // a warning at this many times the reserve (2)
}

// Disk reserve modes.
const (
	ReserveOn      = "on"
	ReserveObserve = "observe"
	ReserveOff     = "off"
)

func def(v *float64, d float64) {
	if *v == 0 {
		*v = d
	}
}

func (g *MemGuard) normalize() {
	def(&g.CommitWarnGB, 10)
	def(&g.RAMWarnPct, 25)
	def(&g.CommitRefuseGB, 6)
	def(&g.RAMRefusePct, 15)
	def(&g.CommitStopGB, 3)
	def(&g.RAMStopPct, 10)
	def(&g.CommitOrphanGB, 1.5)
	def(&g.RAMOrphanPct, 5)
	def(&g.PagingPagesInS, 1000)
	def(&g.PagingSustainS, 60)
	def(&g.PSIAvg10Pct, 10)
	def(&g.HysteresisS, 120)
	def(&g.RestopAfterS, 30)
	def(&g.WarnClearPctMargin, 3)
	def(&g.WarnClearGBMargin, 2)
	def(&g.WarnClearHoldS, 300)
	g.CriticalAgents = slices.DeleteFunc(slices.Clone(g.CriticalAgents), func(s string) bool { return strings.TrimSpace(s) == "" })
	if g.CriticalAgents == nil {
		g.CriticalAgents = []string{"wa-watcher-h"}
	}
}

func (d *DiskReserve) normalize() {
	if d.Mode != ReserveObserve && d.Mode != ReserveOff {
		d.Mode = ReserveOn
	}
	switch {
	case d.GB == 0:
		d.GB = 5
	case d.GB < 0:
		d.GB = 0
	}
	d.Pct = max(d.Pct, 0)
	if d.WarnFactor < 1 {
		d.WarnFactor = 2
	}
}

// Reserve is the bytes to keep free on a volume of total bytes.
func (d DiskReserve) Reserve(total uint64) uint64 {
	const gb = 1 << 30
	return uint64(max(d.GB*gb, float64(total)*d.Pct/100))
}

func (g MemGuard) validate() error {
	pairs := [][2]float64{{g.CommitWarnGB, g.CommitRefuseGB}, {g.CommitRefuseGB, g.CommitStopGB}, {g.CommitStopGB, g.CommitOrphanGB},
		{g.RAMWarnPct, g.RAMRefusePct}, {g.RAMRefusePct, g.RAMStopPct}, {g.RAMStopPct, g.RAMOrphanPct}}
	for _, p := range pairs {
		if p[0] > 0 && p[1] > p[0] {
			return fmt.Errorf("memguard thresholds must fall from warn through refuse and stop to orphan")
		}
	}
	return nil
}

func (d DiskReserve) validate() error {
	if d.Mode != "" && d.Mode != ReserveOn && d.Mode != ReserveObserve && d.Mode != ReserveOff {
		return fmt.Errorf("disk_reserve.mode must be on, observe or off, not %q", d.Mode)
	}
	if d.GB > 100_000 || d.Pct > 100 {
		return fmt.Errorf("disk_reserve is out of range")
	}
	return nil
}

// NormalizeMemGuard fills a MemGuard's defaults (for callers that need the default values).
func NormalizeMemGuard(g *MemGuard) { g.normalize() }
