package config

import (
	"fmt"
	"maps"
	"slices"
	"sort"
)

// Slot kinds that always exist (config may add more).
const (
	SlotBrowser = "browser"
	SlotHeavy   = "heavy"
)

// Resources is config.json "resources": launch limits, priority, sampling, brakes and slots
// (DESIGN.md "Resources"). Zero fields take their defaults (see Normalize); limits that can
// be switched off use 0 for off only where the default is off too, else a negative value.
type Resources struct {
	// Launch queue: a start over a limit waits (state queued) instead of failing.
	MaxAgents    int `json:"max_agents"`      // running agents, the head included; <0 = no limit
	MinFreeRAMMB int `json:"min_free_ram_mb"` // new agents wait while less is free; <0 = off

	Priority   string `json:"priority"`    // "low" (default: nice 10 / below normal) or "normal"
	SampleSecs int    `json:"sample_secs"` // measuring interval, 2..60

	// Brakes: on breach the agent's whole tree is paused (SIGSTOP / NtSuspendProcess).
	DailyWriteGB float64 `json:"daily_write_gb"` // per agent per local day; <0 = off
	RearmGB      float64 `json:"rearm_gb"`       // after a resume: paused again after this much more
	MemoryCapMB  int     `json:"memory_cap_mb"`  // per agent tree; 0 = off (default)
	// Disk saturated: average disk queue ≥ DiskQueue for DiskBusySecs while agents write at
	// least DiskAgentMBs together; the heaviest writer is paused for SaturationPauseSecs.
	DiskQueue           float64 `json:"disk_queue"`            // <0 = off
	DiskBusySecs        int     `json:"disk_busy_secs"`        //
	DiskAgentMBs        float64 `json:"disk_agent_mb_s"`       //
	SaturationPauseSecs int     `json:"saturation_pause_secs"` //

	// MemGuard and DiskReserve: the machine-protection guards (protect.go).
	MemGuard    MemGuard    `json:"memguard"`
	DiskReserve DiskReserve `json:"disk_reserve"`

	// Slots: machine-wide counting semaphores for heavy work (tmx slot).
	Slots map[string]int `json:"slots"`
	// Exempt: agent name → until (Unix ms); exempt agents are never braked.
	Exempt map[string]int64 `json:"exempt,omitempty"`
}

// DefaultResources is the configuration a fresh install runs with.
func DefaultResources() Resources {
	r := Resources{}
	r.Normalize()
	return r
}

// Normalize fills defaults and clamps values into their ranges.
func (r *Resources) Normalize() {
	if r.MaxAgents == 0 {
		r.MaxAgents = 8
	}
	if r.MinFreeRAMMB == 0 {
		r.MinFreeRAMMB = 1500
	}
	if r.Priority != "normal" {
		r.Priority = "low"
	}
	switch {
	case r.SampleSecs == 0:
		r.SampleSecs = 5
	case r.SampleSecs < 2:
		r.SampleSecs = 2
	case r.SampleSecs > 60:
		r.SampleSecs = 60
	}
	if r.DailyWriteGB == 0 {
		r.DailyWriteGB = 20
	}
	if r.RearmGB <= 0 {
		r.RearmGB = 2
	}
	if r.MemoryCapMB < 0 {
		r.MemoryCapMB = 0
	}
	if r.DiskQueue == 0 {
		r.DiskQueue = 8
	}
	if r.DiskBusySecs <= 0 {
		r.DiskBusySecs = 30
	}
	if r.DiskAgentMBs <= 0 {
		r.DiskAgentMBs = 50
	}
	if r.SaturationPauseSecs <= 0 {
		r.SaturationPauseSecs = 60
	}
	slots := map[string]int{SlotBrowser: 2, SlotHeavy: 2}
	for k, v := range r.Slots {
		if v > 0 {
			slots[k] = min(v, 64)
		}
	}
	r.Slots = slots
	r.MemGuard.normalize()
	r.DiskReserve.normalize()
	if len(r.Exempt) == 0 {
		r.Exempt = nil
	}
}

// Validate rejects values a person would not mean.
func (r Resources) Validate() error {
	if r.Priority != "" && r.Priority != "low" && r.Priority != "normal" {
		return fmt.Errorf("priority must be low or normal, not %q", r.Priority)
	}
	for k, v := range r.Slots {
		if k == "" || v < 0 || v > 64 {
			return fmt.Errorf("slot %q must have 1 to 64 places", k)
		}
	}
	if err := r.MemGuard.validate(); err != nil {
		return err
	}
	if err := r.DiskReserve.validate(); err != nil {
		return err
	}
	if r.DailyWriteGB > 100_000 || r.MemoryCapMB > 10_000_000 || r.MaxAgents > 1000 {
		return fmt.Errorf("a limit is out of range")
	}
	return nil
}

// Clone returns a deep copy.
func (r Resources) Clone() Resources {
	r.Slots = maps.Clone(r.Slots)
	r.Exempt = maps.Clone(r.Exempt)
	r.MemGuard.CriticalAgents = slices.Clone(r.MemGuard.CriticalAgents)
	if r.MemGuard.Enabled != nil {
		on := *r.MemGuard.Enabled
		r.MemGuard.Enabled = &on
	}
	return r
}

// SlotKinds returns the configured slot kinds, sorted.
func (r Resources) SlotKinds() []string {
	out := make([]string, 0, len(r.Slots))
	for k := range r.Slots {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// EffectiveResources returns config.json's resources block with defaults filled in.
func (c Config) EffectiveResources() Resources {
	var r Resources
	if c.Resources != nil {
		r = c.Resources.Clone()
	}
	r.Normalize()
	return r
}
