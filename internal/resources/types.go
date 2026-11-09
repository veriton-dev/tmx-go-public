package resources

import (
	"github.com/blackcode-ch/tmx-go/internal/config"
	"github.com/blackcode-ch/tmx-go/internal/slots"
)

// Sample is one measurement of the machine and the agents (bus event resources.sampled).
type Sample struct {
	At       int64        `json:"at"`
	Interval int          `json:"interval"` // seconds between samples
	Machine  MachineUsage `json:"machine"`
	Agents   []AgentUsage `json:"agents"` // live agents, plus stopped ones that wrote today
}

// MachineUsage is the machine strip.
type MachineUsage struct {
	CPU            float64 `json:"cpu"` // percent of all cores
	Cores          int     `json:"cores"`
	MemTotal       uint64  `json:"mem_total"`
	MemFree        uint64  `json:"mem_free"` // available for new work
	DiskFree       uint64  `json:"disk_free"`
	DiskTotal      uint64  `json:"disk_total"`
	DiskPath       string  `json:"disk_path"`       // the workspace, whose volume is measured
	DiskQueue      float64 `json:"disk_queue"`      // average requests in flight, busiest disk; -1 = not measured
	DiskWriteRate  float64 `json:"disk_write_rate"` // bytes/s by every program; -1 = not measured
	AgentWriteRate float64 `json:"agent_write_rate"`
	Saturated      bool    `json:"saturated"` // the disk-saturated condition holds now
	WrittenToday   uint64  `json:"written_today"`
	Running        int     `json:"running"`
	Queued         int     `json:"queued"`
	Paused         int     `json:"paused"`
}

// AgentUsage is one agent's process tree.
type AgentUsage struct {
	AgentID      string  `json:"agent_id"`
	CPU          float64 `json:"cpu"` // percent of one core
	Mem          uint64  `json:"mem"`
	WriteRate    float64 `json:"write_rate"` // bytes/s
	WrittenToday uint64  `json:"written_today"`
	Budget       uint64  `json:"budget"` // bytes it may write today before a pause; 0 = no budget
	Procs        int     `json:"procs"`
	ExemptUntil  int64   `json:"exempt_until,omitempty"`
}

// Point is one history entry: CPU %, memory (machine: free) and write rate.
type Point struct {
	T   int64   `json:"t"`
	CPU float32 `json:"c"`
	Mem uint64  `json:"m"`
	W   float32 `json:"w"`
}

// History keeps the last HistoryLen samples for sparklines.
type History struct {
	Machine []Point            `json:"machine"`
	Agents  map[string][]Point `json:"agents"`
}

// Cost is what the sampler itself costs (wall time per tick: an upper bound of its CPU).
type Cost struct {
	Ticks  int     `json:"ticks"`
	AvgMs  float64 `json:"avg_ms"`
	MaxMs  float64 `json:"max_ms"`
	CPUPct float64 `json:"cpu_pct"` // avg tick time over the interval, in percent of one core
}

// Support says what this build can measure.
type Support struct {
	OS         string `json:"os"`
	PerProcess bool   `json:"per_process"` // CPU, memory and disk writes per agent
	DiskQueue  bool   `json:"disk_queue"`  // the disk-saturated brake
}

// View is GET /api/resources.
type View struct {
	Config  config.Resources `json:"config"`
	Sample  Sample           `json:"sample"`
	History History          `json:"history"`
	Slots   slots.View       `json:"slots"`
	Cost    Cost             `json:"cost"`
	Protect ProtectView      `json:"protect"`
	Support Support          `json:"support"`
}
