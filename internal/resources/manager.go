// Package resources is the daemon's resource control (DESIGN.md "Resources"): it samples
// every agent's process tree and the machine every few seconds, keeps the day's write
// accounting, admits starts for the launch queue, and brakes (pauses) an agent's tree that
// breaks its daily write budget or memory cap, or writes the most while the disk is
// saturated. The fleet does the pausing; this package decides.
package resources

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blackcode-ch/tmx-go/internal/config"
	"github.com/blackcode-ch/tmx-go/internal/model"
	"github.com/blackcode-ch/tmx-go/internal/slots"
	"github.com/blackcode-ch/tmx-go/internal/sysinfo"
)

const (
	MB = 1 << 20
	GB = 1 << 30
	// HistoryLen is how many samples the sparklines keep (5 minutes at 5 s).
	HistoryLen = 60
	// admitReserve is what one just-admitted agent is assumed to take before the next
	// sample shows it, so a burst of starts cannot all pass one free-RAM reading.
	admitReserve = 512 * MB
	// minWriterRate: an agent writing less is never the one paused for a saturated disk.
	minWriterRate = 1 * MB
	saveEvery     = time.Minute
)

// Fleet is what resource control needs from the fleet (implemented by *fleet.Fleet).
type Fleet interface {
	LiveAgents() []model.Agent
	Counts() (running, queued, paused int)
	BrakeAgent(id, reason, detail string, ask bool) ([]sysinfo.Proc, error)
	UnbrakeAgent(id, reason, why string) bool
	PumpQueue()
	Publish(model.BusEvent)
}

// Brake reasons (the fleet's Agent.PausedReason values).
const (
	BrakeWriteBudget = "write_budget"
	BrakeMemory      = "memory"
	BrakeDisk        = "disk"
)

// Options configures a Manager.
type Options struct {
	Paths     config.Paths
	Config    config.Resources
	Workspace string                       // the volume whose free space is shown
	Save      func(config.Resources) error // persists a changed config (config.json); nil = memory only
	Slots     *slots.Manager               // sizes follow the config; nil = no slots
	Sys       Sys                          // nil = this machine
	TempDir   string                       // the temp folder whose volume is watched; "" = the OS temp dir
	Now       func() time.Time
	Logger    *slog.Logger
}

// Manager is resource control. New, Attach(fleet), Start, go Run(ctx), Close.
type Manager struct {
	watchRestored sync.Map // IDs restored without acquiring the governor mutex
	lastCompleted atomic.Int64
	staleAfter    atomic.Int64
	opts          Options
	sys           Sys
	log           *slog.Logger
	fleet         Fleet

	mu       sync.Mutex
	cfg      config.Resources
	led      ledger
	dirty    bool
	savedAt  time.Time
	sample   Sample
	sampled  time.Time
	admitted int // admissions since the last sample

	prev        map[int]procPrev
	prevAt      time.Time
	prevMachine sysinfo.Machine
	prevDisks   map[string]sysinfo.Disk
	satSince    time.Time            // disk saturated since; zero = not
	diskTurns   map[string]time.Time // agent ID → its disk pause ends
	history     History
	cost        Cost
	totalMs     float64
	wake        chan struct{}
	prot        protectState
}

type procPrev struct {
	start   int64
	cpu     time.Duration
	written uint64
}

// New creates a Manager (nothing runs until Start and Run).
func New(o Options) *Manager {
	if o.Sys == nil {
		o.Sys = osSys{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	cfg := o.Config.Clone()
	cfg.Normalize()
	m := &Manager{opts: o, sys: o.Sys, log: o.Logger, cfg: cfg, led: ledger{}.fill(),
		prev: map[int]procPrev{}, prevDisks: map[string]sysinfo.Disk{}, diskTurns: map[string]time.Time{},
		history: History{Agents: map[string][]Point{}}, wake: make(chan struct{}, 1),
		prot: protectState{vols: map[string]*volState{}, read: map[string]volReading{}, braked: map[string]time.Time{}}}
	if o.Slots != nil {
		o.Slots.SetSizes(cfg.Slots)
	}
	return m
}

// Attach connects the fleet (before Start).
func (m *Manager) Attach(f Fleet) { m.fleet = f }

// Start loads today's ledger and continues processes a crashed daemon left paused (only
// those still running with the same start time). Call it before the fleet starts.
func (m *Manager) Start() error {
	led, err := loadLedger(m.opts.Paths)
	if err != nil {
		m.log.Warn("resources.json unreadable; starting the day's accounting afresh", "err", err)
	}
	for id, procs := range led.Paused {
		if n := m.sys.ResumeProcs(procs); n > 0 {
			m.log.Warn("continued processes a previous daemon left paused", "agent", id, "processes", n)
		}
	}
	led.Paused = map[string][]sysinfo.Proc{}
	if day := m.opts.Now().Format(time.DateOnly); led.Day != day {
		led.newDay(day)
	}
	m.mu.Lock()
	m.led = led
	m.dirty = true
	m.mu.Unlock()
	m.save(true)
	return nil
}

// Run samples every SampleSecs until ctx ends, then saves the ledger.
func (m *Manager) Run(ctx context.Context) {
	defer func() {
		m.lastCompleted.Store(0)
		if recover() != nil {
			m.log.Error("governor degraded: sampler panicked")
		}
	}()
	interval := m.interval()
	t := time.NewTicker(interval)
	defer t.Stop()
	m.Tick()
	for {
		select {
		case <-ctx.Done():
			m.save(true)
			return
		case <-t.C:
		case <-m.wake:
		}
		m.Tick()
		if iv := m.interval(); iv != interval {
			interval = iv
			t.Reset(iv)
		}
	}
}

// Fresh is a lock-free completed-sample observation for job admission and health.
// A wedged sampler cannot hold up cancellation or status through this method.
func (m *Manager) Fresh() bool {
	last := m.lastCompleted.Load()
	return last != 0 && time.Since(time.Unix(0, last)) < time.Duration(m.staleAfter.Load())
}

// Close saves the ledger.
func (m *Manager) Close() { m.save(true) }

func (m *Manager) interval() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return time.Duration(m.cfg.SampleSecs) * time.Second
}

// Config returns the effective configuration.
func (m *Manager) Config() config.Resources {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Clone()
}

// SetConfig validates, persists and applies a new configuration (owner).
func (m *Manager) SetConfig(c config.Resources) (config.Resources, error) {
	if err := c.Validate(); err != nil {
		return config.Resources{}, err
	}
	c = c.Clone()
	c.Normalize()
	if m.opts.Save != nil {
		if err := m.opts.Save(c); err != nil {
			return config.Resources{}, err
		}
	}
	m.mu.Lock()
	m.cfg = c
	m.mu.Unlock()
	if m.opts.Slots != nil {
		m.opts.Slots.SetSizes(c.Slots)
	}
	m.poke()
	return c.Clone(), nil
}

// SetExempt exempts an agent (by name) from every brake until until (Unix ms; 0 = remove).
func (m *Manager) SetExempt(name string, until int64) (config.Resources, error) {
	c := m.Config()
	if c.Exempt == nil {
		c.Exempt = map[string]int64{}
	}
	if until <= 0 {
		delete(c.Exempt, name)
	} else {
		c.Exempt[name] = until
	}
	now := m.opts.Now().UnixMilli()
	for k, v := range c.Exempt {
		if v <= now {
			delete(c.Exempt, k) // expired
		}
	}
	return m.SetConfig(c)
}

// poke asks Run for a sample now (after a config change).
func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Admit implements fleet.Resources: one more agent may start while running agents run,
// unless that reaches max_agents or free RAM is below min_free_ram_mb.
func (m *Manager) Admit(running int) (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.cfg
	if c.MaxAgents > 0 && running >= c.MaxAgents {
		return false, fmt.Sprintf("%d of %d agents running", running, c.MaxAgents)
	}
	free := m.sample.Machine.MemFree
	fresh := m.freshLocked()
	if why := m.admitBlockLocked(); why != "" {
		return false, why
	}
	if c.MinFreeRAMMB > 0 && free > 0 && fresh {
		left := int64(free) - int64(m.admitted)*admitReserve
		if left < int64(c.MinFreeRAMMB)*MB {
			return false, fmt.Sprintf("%s RAM free, below the %s minimum", Bytes(max(left, 0)), Bytes(int64(c.MinFreeRAMMB)*MB))
		}
	}
	m.admitted++
	return true, ""
}

// freshLocked: the last sample is recent enough to admit on.
func (m *Manager) freshLocked() bool {
	return !m.sampled.IsZero() && m.opts.Now().Sub(m.sampled) < 3*time.Duration(m.cfg.SampleSecs)*time.Second
}

// LowPriority implements fleet.Resources.
func (m *Manager) LowPriority() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Priority != "normal"
}

// Resumed implements fleet.Resources: the owner resumed an agent braked for reason.
// Resume rearms the brake after RearmGB more; raise adds another day's budget.
func (m *Manager) Resumed(agentID, reason string, raise bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch reason {
	case BrakeWriteBudget:
		if raise {
			m.led.Extra[agentID] += uint64(m.cfg.DailyWriteGB * GB)
			delete(m.led.Rearm, agentID)
		} else {
			m.led.Rearm[agentID] = m.led.Written[agentID] + uint64(m.cfg.RearmGB*GB)
		}
	case BrakeMemory:
		m.led.MemOK[agentID] = true
	case BrakeDisk:
		delete(m.diskTurns, agentID)
	case BrakeMemGuard:
		delete(m.prot.braked, agentID)
	}
	delete(m.led.Paused, agentID)
	m.dirty = true
}

// View is GET /api/resources.
func (m *Manager) View() View {
	m.mu.Lock()
	v := View{Config: m.cfg.Clone(), Sample: cloneSample(m.sample), Cost: m.cost, Protect: m.protectViewLocked(),
		History: History{Machine: append([]Point(nil), m.history.Machine...), Agents: map[string][]Point{}},
		Support: Support{OS: runtime.GOOS, PerProcess: sysinfo.PerProcess, DiskQueue: m.sample.Machine.DiskQueue >= 0}}
	for id, h := range m.history.Agents {
		v.History.Agents[id] = append([]Point(nil), h...)
	}
	m.mu.Unlock()
	if v.History.Machine == nil {
		v.History.Machine = []Point{}
	}
	if m.opts.Slots != nil {
		v.Slots = m.opts.Slots.View()
	} else {
		v.Slots = slots.View{Kinds: []slots.KindView{}, Leases: []slots.Lease{}}
	}
	return v
}

func cloneSample(s Sample) Sample {
	s.Agents = append([]AgentUsage{}, s.Agents...)
	return s
}

func (m *Manager) save(force bool) {
	m.mu.Lock()
	if !m.dirty || (!force && m.opts.Now().Sub(m.savedAt) < saveEvery) {
		m.mu.Unlock()
		return
	}
	led := ledger{Day: m.led.Day, Written: clone(m.led.Written), Extra: clone(m.led.Extra),
		Rearm: clone(m.led.Rearm), MemOK: clone(m.led.MemOK), Paused: clone(m.led.Paused)}
	m.dirty, m.savedAt = false, m.opts.Now()
	m.mu.Unlock()
	if err := led.save(m.opts.Paths); err != nil {
		m.log.Warn("save resources.json", "err", err)
	}
}

func clone[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Bytes formats a byte count for people: "1.4 GB", "512 MB", "0 B".
func Bytes(n int64) string {
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1f GB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.0f MB", float64(n)/MB)
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
