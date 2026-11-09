package resources

// Machine protection: the memory guard and the per-volume free-space reserve (parity with
// Python tmx's memguard.py and ioguard.VolumeReserve). The decision logic here is pure
// (readings in, levels and victims out); Manager feeds it every tick and applies the result
// through the fleet's brake, which suspends the PID tree tmx owns. Nothing is ever killed.

import (
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/blackcode-ch/tmx-go/internal/config"
	"github.com/blackcode-ch/tmx-go/internal/model"
	"github.com/blackcode-ch/tmx-go/internal/sysinfo"
)

// Brake reasons of the two guards (the fleet's Agent.PausedReason values).
const (
	BrakeMemGuard    = "memguard"
	BrakeDiskReserve = "disk_reserve"
)

// Level is the memory guard's state; later in the list is worse.
type Level string

const (
	LevelOK     Level = "ok"
	LevelWarn   Level = "warn"   // alert only
	LevelRefuse Level = "refuse" // no new agent starts
	LevelStop   Level = "stop"   // agents are paused one by one
	LevelOrphan Level = "orphan" // acts as stop (orphan cleanup is not done: nothing is killed)
)

var levelRank = map[Level]int{LevelOK: 0, LevelWarn: 1, LevelRefuse: 2, LevelStop: 3, LevelOrphan: 4}

// pagingRAMGate: sustained paging is only trusted with RAM already this low (percent).
const pagingRAMGate = 20.0

// MemReading is what the memory guard judges.
type MemReading struct {
	CommitGB    float64 `json:"free_commit_gb"`
	CommitKnown bool    `json:"commit_known"`
	RAMPct      float64 `json:"ram_available_pct"`
	RAMKnown    bool    `json:"ram_known"`
	PagingHot   bool    `json:"paging_hot"` // sustained, see guardState.update
}

func below(v, floor float64) bool { return floor > 0 && v < floor }

// memLevel is the worse of the two signals at every level, as in Python's Reading.level.
func memLevel(r MemReading, c config.MemGuard) Level {
	gb := func(floor float64) bool { return r.CommitKnown && below(r.CommitGB, floor) }
	ram := func(floor float64) bool { return r.RAMKnown && below(r.RAMPct, floor) }
	switch {
	case gb(c.CommitOrphanGB) || ram(c.RAMOrphanPct):
		return LevelOrphan
	case gb(c.CommitStopGB) || ram(c.RAMStopPct) || (r.PagingHot && ram(pagingRAMGate)):
		return LevelStop
	case gb(c.CommitRefuseGB) || ram(c.RAMRefusePct):
		return LevelRefuse
	case gb(c.CommitWarnGB) || ram(c.RAMWarnPct):
		return LevelWarn
	}
	return LevelOK
}

// scaledMemGuard caps the commit floors a person left at their default to a share of this
// machine's RAM when the reader reports RAM-plus-swap headroom, so an 8 GB laptop does
// not inherit floors tuned for a 64 GB Windows box. Absolute commit-limit readings are
// never scaled. A floor set to its default value counts as unset.
func scaledMemGuard(c config.MemGuard, totalRAMGB float64, ramBased bool) config.MemGuard {
	if !ramBased || totalRAMGB <= 0 {
		return c
	}
	d := config.MemGuard{}
	config.NormalizeMemGuard(&d)
	for _, f := range []struct {
		v        *float64
		def, pct float64
	}{{&c.CommitWarnGB, d.CommitWarnGB, 20}, {&c.CommitRefuseGB, d.CommitRefuseGB, 12},
		{&c.CommitStopGB, d.CommitStopGB, 6}, {&c.CommitOrphanGB, d.CommitOrphanGB, 3}} {
		if *f.v != f.def {
			continue
		}
		if s := float64(int(totalRAMGB*f.pct)) / 100; s < f.def {
			*f.v = s
		}
	}
	return c
}

// guardState is the memory guard's memory between ticks.
type guardState struct {
	level        Level // effective (with the warn-clear hysteresis)
	raw          Level
	reading      MemReading
	pressure     time.Time // sustained-paging clock: since when the signal is hot
	clearSince   time.Time // since when the warn-clear conditions hold
	episodeSince time.Time // a stop episode: since its first stop-level tick
	lastStop     time.Time // the last pause it ordered
	lastAlert    Level
}

// update takes one reading and returns the effective level. The warn level is sticky:
// after any non-ok level it clears only when RAM is ram_warn_pct+margin AND free commit is
// commit_warn_gb+margin, both held for warn_clear_hold_s, so it cannot flap at the line.
func (g *guardState) update(now time.Time, c config.MemGuard, mach sysinfo.Machine) Level {
	r := MemReading{CommitGB: float64(mach.CommitFree) / GB, CommitKnown: mach.CommitKnown}
	if mach.MemTotal > 0 {
		r.RAMPct, r.RAMKnown = float64(mach.MemFree)/float64(mach.MemTotal)*100, true
	}
	hot := false
	switch mach.Pressure.Kind {
	case sysinfo.PressurePagesIn:
		hot = mach.Pressure.Value >= c.PagingPagesInS
	case sysinfo.PressurePSI:
		hot = mach.Pressure.Value >= c.PSIAvg10Pct
	}
	switch {
	case !hot:
		g.pressure = time.Time{}
	case g.pressure.IsZero():
		g.pressure = now
	}
	r.PagingHot = hot && now.Sub(g.pressure) >= time.Duration(c.PagingSustainS*float64(time.Second))
	g.reading = r
	g.raw = memLevel(r, c)
	eff := g.raw
	if eff == LevelOK && g.level != LevelOK && g.level != "" {
		clear := (!r.RAMKnown || r.RAMPct >= c.RAMWarnPct+c.WarnClearPctMargin) &&
			(!r.CommitKnown || r.CommitGB >= c.CommitWarnGB+c.WarnClearGBMargin)
		switch {
		case !clear:
			g.clearSince = time.Time{}
			eff = LevelWarn
		case g.clearSince.IsZero():
			g.clearSince = now
			eff = LevelWarn
		case now.Sub(g.clearSince) < time.Duration(c.WarnClearHoldS*float64(time.Second)):
			eff = LevelWarn
		}
	}
	if eff != LevelWarn || g.raw != LevelOK {
		g.clearSince = time.Time{}
	}
	g.level = eff
	if levelRank[eff] >= levelRank[LevelStop] {
		if g.episodeSince.IsZero() || now.Sub(g.episodeSince) >= time.Duration(c.HysteresisS*float64(time.Second)) {
			g.episodeSince = now // the hysteresis cap while still under pressure: a fresh episode
		}
	} else if eff == LevelOK {
		g.episodeSince = time.Time{}
	}
	return eff
}

// victim is one agent the guard may pause.
type victim struct {
	id, name string
	group    int
	mem      uint64
}

// selectVictims orders the agents to pause: idle workers, then working workers, then leads;
// the largest process tree first within a group. The head, the owner shell, critical names,
// exempt and already paused agents are never candidates. Pure.
func selectVictims(trees []tree, usage []AgentUsage, c config.MemGuard, exempt map[string]int64, nowMs int64) []victim {
	var out []victim
	for i, t := range trees {
		a := t.agent
		if a.Level == model.LevelHead || a.Harness == model.HarnessShell || a.State == model.StatePaused ||
			exempt[a.Name] > nowMs || slices.ContainsFunc(c.CriticalAgents, func(n string) bool { return strings.EqualFold(n, a.Name) }) {
			continue
		}
		g := 1
		switch {
		case a.Level == model.LevelLead:
			g = 2
		case a.State == model.StateIdle:
			g = 0
		}
		out = append(out, victim{id: a.ID, name: a.Name, group: g, mem: usage[i].Mem})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].group != out[j].group {
			return out[i].group < out[j].group
		}
		return out[i].mem > out[j].mem
	})
	return out
}

// Volume reserve ------------------------------------------------------------------------

// Volume levels.
const (
	VolOK   = "ok"
	VolWarn = "warn" // under warn_factor × reserve
	VolLow  = "low"  // under the reserve
)

type volReading struct{ free, total uint64 }

// volState is the reserve's memory of one volume.
type volState struct {
	level   string
	last    uint64
	hasLast bool
	braked  map[string]bool // agents braked for this volume in the current episode
	alerted bool            // the exempt-writer alert was raised in this episode
}

// GuardEvent is one thing the guards noticed or did (bus event resources.guard).
type GuardEvent struct {
	At     int64  `json:"at"`
	Kind   string `json:"kind"` // memguard-<level> | memguard-pause | disk-reserve-<level> | disk-reserve-brake | disk-reserve-would-brake
	Agent  string `json:"agent,omitempty"`
	Volume string `json:"volume,omitempty"`
	Detail string `json:"detail"`
}

// reserveBrake is one agent to pause for one volume.
type reserveBrake struct {
	volume, agentID, agent string
	rate                   float64
	reading                volReading
	reserve                uint64
}

// reserveTick is Python's VolumeReserve.tick: per volume the level, an event on each change
// and, while the volume is under the reserve and still falling, the one agent that writes
// most to it (not yet braked in this episode). writers: volume → agent ID → bytes/s of agents
// the brake may pause; exempt holds the same for agents it never pauses (the head, critical
// and exempt agents): if only those write, an alert is raised and nobody is paused.
func (m *Manager) reserveTick(now time.Time, c config.DiskReserve, vols map[string]volReading,
	writers, exempt map[string]map[string]float64, names map[string]string) []reserveBrake {
	var out []reserveBrake
	if c.Mode == config.ReserveOff {
		return nil
	}
	p := &m.prot
	for v, r := range vols {
		st := p.vols[v]
		if st == nil {
			st = &volState{level: VolOK, braked: map[string]bool{}}
			p.vols[v] = st
		}
		res := c.Reserve(r.total)
		falling := !st.hasLast || r.free < st.last
		st.last, st.hasLast = r.free, true
		level := VolOK
		switch {
		case r.free < res:
			level = VolLow
		case float64(r.free) < float64(res)*c.WarnFactor:
			level = VolWarn
		}
		info := fmt.Sprintf("%s has %s free; the reserve is %s", v, Bytes(int64(r.free)), Bytes(int64(res)))
		if level != st.level {
			p.event(now, "disk-reserve-"+level, "", v, info)
			if level == VolOK {
				st.alerted = false
				st.braked = map[string]bool{}
			}
		}
		st.level = level
		if level != VolLow || !falling {
			continue
		}
		ids := make([]string, 0, len(writers[v]))
		for id, w := range writers[v] {
			if w > 0 && !st.braked[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			if top := topWriter(exempt[v], names); top != "" && !st.alerted {
				st.alerted = true
				p.event(now, "disk-reserve-exempt-writer", top, v, fmt.Sprintf("%s; only exempt agents write to it (%s is the top one) so nobody is paused: free space by hand", info, top))
			}
			continue
		}
		sort.Slice(ids, func(i, j int) bool {
			if writers[v][ids[i]] != writers[v][ids[j]] {
				return writers[v][ids[i]] > writers[v][ids[j]]
			}
			return ids[i] < ids[j]
		})
		id := ids[0]
		w := writers[v][id]
		if c.Mode == config.ReserveObserve {
			p.event(now, "disk-reserve-would-brake", names[id], v, fmt.Sprintf("%s; %s writes the most to it (%s/s): observe mode, not paused", info, names[id], Bytes(int64(w))))
			continue
		}
		st.braked[id] = true
		out = append(out, reserveBrake{volume: v, agentID: id, agent: names[id], rate: w, reading: r, reserve: res})
	}
	return out
}

// topWriter is the name of the agent that writes most, or "" when none does.
func topWriter(w map[string]float64, names map[string]string) string {
	best, id := 0.0, ""
	for k, r := range w {
		if r > best || (r == best && r > 0 && k < id) {
			best, id = r, k
		}
	}
	if id == "" {
		return ""
	}
	return names[id]
}

// volumeKey names the volume a path lives on: the drive letter on Windows, else the path.
func volumeKey(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if runtime.GOOS == "windows" {
		return strings.ToUpper(filepath.VolumeName(path))
	}
	return path
}

// protectState is what the two guards keep. Guarded by Manager.mu.
type protectState struct {
	guard   guardState
	vols    map[string]*volState
	read    map[string]volReading // this tick's readings
	events  []GuardEvent          // the last few, for the status
	pending []GuardEvent          // not yet published
	braked  map[string]time.Time  // agent ID → paused by the memory guard at
}

const keepEvents = 20

func (p *protectState) event(now time.Time, kind, agent, vol, detail string) {
	e := GuardEvent{At: now.UnixMilli(), Kind: kind, Agent: agent, Volume: vol, Detail: detail}
	p.pending = append(p.pending, e)
	p.events = append(p.events, e)
	if over := len(p.events) - keepEvents; over > 0 {
		p.events = append(p.events[:0], p.events[over:]...)
	}
}

// protectLocked runs both guards for this tick and returns the brakes to apply.
func (m *Manager) protectLocked(now time.Time, mach sysinfo.Machine, trees []tree, usage []AgentUsage, taken map[string]bool) []action {
	var acts []action
	c := m.cfg
	nowMs := now.UnixMilli()
	p := &m.prot

	// Memory guard.
	if c.MemGuard.On() {
		mg := scaledMemGuard(c.MemGuard, float64(mach.MemTotal)/GB, mach.CommitRAMBased)
		prev := p.guard.level
		lvl := p.guard.update(now, mg, mach)
		if lvl != prev && !(prev == "" && lvl == LevelOK) {
			p.event(now, "memguard-"+string(lvl), "", "", describeMem(p.guard.reading, lvl))
		}
		if levelRank[lvl] >= levelRank[LevelStop] && (p.guard.lastStop.IsZero() ||
			now.Sub(p.guard.lastStop) >= time.Duration(mg.RestopAfterS*float64(time.Second))) {
			for _, v := range selectVictims(trees, usage, mg, c.Exempt, nowMs) {
				if taken[v.id] {
					continue
				}
				p.guard.lastStop = now
				id, name := v.id, v.name
				detail := fmt.Sprintf("The machine is low on memory (%s); %s uses %s and is paused first, not stopped. Resume it when memory is back.",
					describeMem(p.guard.reading, lvl), name, Bytes(int64(v.mem)))
				acts = append(acts, action{brake: true, id: id, name: name, reason: BrakeMemGuard, ask: true, detail: detail,
					onDone: func(ok bool) {
						m.mu.Lock()
						defer m.mu.Unlock()
						if ok {
							m.prot.braked[id] = m.opts.Now()
						} else {
							m.prot.guard.lastStop = time.Time{} // nothing was paused: try the next one now
						}
					}})
				taken[id] = true
				p.event(now, "memguard-pause", name, "", detail)
				break
			}
		}
	} else {
		p.guard = guardState{}
	}

	// Disk reserve.
	dr := c.DiskReserve
	if dr.Mode == config.ReserveOff || len(p.read) == 0 {
		return acts
	}
	temp := volumeKey(m.tempDir())
	writers := map[string]map[string]float64{}
	exemptW := map[string]map[string]float64{}
	names := map[string]string{}
	for i, t := range trees {
		a := t.agent
		if a.Harness == model.HarnessShell || usage[i].WriteRate <= 0 {
			continue
		}
		// The same exemptions as the memory guard: the head, critical and exempt agents.
		into := writers
		if a.Level == model.LevelHead || c.Exempt[a.Name] > nowMs ||
			slices.ContainsFunc(c.MemGuard.CriticalAgents, func(n string) bool { return strings.EqualFold(n, a.Name) }) {
			into = exemptW
		}
		names[a.ID] = a.Name
		for _, v := range []string{volumeKey(a.Cwd), temp} {
			if v == "" {
				continue
			}
			if into[v] == nil {
				into[v] = map[string]float64{}
			}
			into[v][a.ID] = usage[i].WriteRate
		}
	}
	for _, b := range m.reserveTick(now, dr, p.read, writers, exemptW, names) {
		if taken[b.agentID] {

			continue
		}
		taken[b.agentID] = true
		vol, id := b.volume, b.agentID
		detail := fmt.Sprintf("%s is below its free-space reserve (%s free, %s kept) and %s writes the most to it now (%s/s); paused so the disk does not fill. Resume it when there is room.",
			vol, Bytes(int64(b.reading.free)), Bytes(int64(b.reserve)), b.agent, Bytes(int64(b.rate)))
		acts = append(acts, action{brake: true, id: id, name: b.agent, reason: BrakeDiskReserve, ask: true, detail: detail,
			onDone: func(ok bool) {
				m.mu.Lock()
				defer m.mu.Unlock()
				if !ok {
					if st := m.prot.vols[vol]; st != nil {
						delete(st.braked, id) // not paused: the next tick may pick it again
					}
				}
			}})
		p.event(now, "disk-reserve-brake", b.agent, vol, detail)
	}
	return acts
}

func (m *Manager) tempDir() string {
	if m.opts.TempDir != "" {
		return m.opts.TempDir
	}
	return tempDir()
}

func describeMem(r MemReading, l Level) string {
	var bits []string
	if r.CommitKnown {
		bits = append(bits, fmt.Sprintf("%.1f GB free commit", r.CommitGB))
	}
	if r.RAMKnown {
		bits = append(bits, fmt.Sprintf("%.0f%% RAM available", r.RAMPct))
	}
	if r.PagingHot {
		bits = append(bits, "sustained hard paging")
	}
	s := strings.Join(bits, ", ")
	if s == "" {
		s = "no reading"
	}
	return fmt.Sprintf("memory level %s: %s", l, s)
}

// admitBlock is why the guards forbid a new agent now ("" = none). Held under m.mu.
func (m *Manager) admitBlockLocked() string {
	if !m.freshLocked() {
		return ""
	}
	p := &m.prot
	if m.cfg.MemGuard.On() && levelRank[p.guard.level] >= levelRank[LevelRefuse] {
		return fmt.Sprintf("the machine is low on memory (%s): no new agents until it recovers", describeMem(p.guard.reading, p.guard.level))
	}
	if m.cfg.DiskReserve.Mode == config.ReserveOn {
		for _, v := range sortedKeys(p.vols) {
			if st := p.vols[v]; st.level == VolLow {
				r := p.read[v]
				return fmt.Sprintf("%s is below its free-space reserve (%s free, %s kept): no new agents until there is room",
					v, Bytes(int64(r.free)), Bytes(int64(m.cfg.DiskReserve.Reserve(r.total))))
			}
		}
	}
	return ""
}

// ReserveBlock says whether writing a new copy of needed bytes under path would break its
// volume's reserve: callers that pack or copy a tree ask first (false = refuse, with why).
func (m *Manager) ReserveBlock(path string, needed uint64) (ok bool, why string) {
	m.mu.Lock()
	c := m.cfg.DiskReserve
	m.mu.Unlock()
	if c.Mode != config.ReserveOn {
		return true, ""
	}
	free, total, err := m.sys.DiskSpace(path)
	if err != nil {
		return true, "" // unreadable: the guard cannot judge, the write decides
	}
	res := c.Reserve(total)
	if free < res+needed {
		return false, fmt.Sprintf("%s has %s free and keeps %s in reserve; %s more would break it",
			volumeKey(path), Bytes(int64(free)), Bytes(int64(res)), Bytes(int64(needed)))
	}
	return true, ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Status view -----------------------------------------------------------------------------

// ProtectView is the owner-visible state of both guards (GET /api/resources, "protect").
type ProtectView struct {
	MemGuard    MemGuardView    `json:"memguard"`
	DiskReserve DiskReserveView `json:"disk_reserve"`
	Events      []GuardEvent    `json:"events"`
}

// MemGuardView is the memory guard's state.
type MemGuardView struct {
	Enabled bool       `json:"enabled"`
	Level   Level      `json:"level"` // effective, with the warn-clear hysteresis
	Raw     Level      `json:"raw"`   // the instantaneous reading's level
	Reading MemReading `json:"reading"`
	Stopped []string   `json:"paused,omitempty"` // agent IDs it paused and that stay paused
}

// DiskReserveView is the volume reserve's state.
type DiskReserveView struct {
	Mode    string       `json:"mode"`
	State   string       `json:"state"` // the worst volume's level
	Volumes []VolumeView `json:"volumes"`
}

// VolumeView is one watched volume.
type VolumeView struct {
	Volume    string   `json:"volume"`
	FreeGB    float64  `json:"free_gb"`
	TotalGB   float64  `json:"total_gb"`
	ReserveGB float64  `json:"reserve_gb"`
	Level     string   `json:"level"`
	Braked    []string `json:"braked,omitempty"` // agent IDs braked for it in this episode
}

func (m *Manager) protectViewLocked() ProtectView {
	p := &m.prot
	v := ProtectView{Events: append([]GuardEvent{}, p.events...)}
	v.MemGuard = MemGuardView{Enabled: m.cfg.MemGuard.On(), Level: p.guard.level, Raw: p.guard.raw, Reading: p.guard.reading}
	if v.MemGuard.Level == "" {
		v.MemGuard.Level, v.MemGuard.Raw = LevelOK, LevelOK
	}
	for id := range p.braked {
		v.MemGuard.Stopped = append(v.MemGuard.Stopped, id)
	}
	sort.Strings(v.MemGuard.Stopped)
	v.DiskReserve = DiskReserveView{Mode: m.cfg.DiskReserve.Mode, State: VolOK, Volumes: []VolumeView{}}
	for _, k := range sortedKeys(p.vols) {
		st, r := p.vols[k], p.read[k]
		vv := VolumeView{Volume: k, FreeGB: round1(float64(r.free) / GB), TotalGB: round1(float64(r.total) / GB),
			ReserveGB: round1(float64(m.cfg.DiskReserve.Reserve(r.total)) / GB), Level: st.level}
		for id, on := range st.braked {
			if on {
				vv.Braked = append(vv.Braked, id)
			}
		}
		sort.Strings(vv.Braked)
		if st.level == VolLow || (st.level == VolWarn && v.DiskReserve.State == VolOK) {
			v.DiskReserve.State = st.level
		}
		v.DiskReserve.Volumes = append(v.DiskReserve.Volumes, vv)
	}
	return v
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// Health is the short public form for /api/health: levels only, no names.
type Health struct {
	MemGuard    HealthMem  `json:"memguard"`
	DiskReserve HealthDisk `json:"disk_reserve"`
}

// HealthMem is the memory guard in /api/health.
type HealthMem struct {
	Enabled bool  `json:"enabled"`
	Level   Level `json:"level"`
}

// HealthDisk is the disk reserve in /api/health.
type HealthDisk struct {
	Mode  string `json:"mode"`
	State string `json:"state"`
}

// Health returns the guards' levels.
func (m *Manager) Health() Health {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.protectViewLocked()
	return Health{MemGuard: HealthMem{Enabled: v.MemGuard.Enabled, Level: v.MemGuard.Level},
		DiskReserve: HealthDisk{Mode: v.DiskReserve.Mode, State: v.DiskReserve.State}}
}

// readVolumes measures every watched volume: the workspace, tmx's home, the temp folder
// and where each live agent works. Outside the lock (it asks the OS).
func (m *Manager) readVolumes(workspace string, agents []model.Agent) map[string]volReading {
	m.mu.Lock()
	mode := m.cfg.DiskReserve.Mode
	m.mu.Unlock()
	out := map[string]volReading{}
	if mode == config.ReserveOff {
		return out
	}
	probe := map[string]string{} // volume → a path on it
	for _, p := range append([]string{workspace, m.opts.Paths.Home, m.tempDir()}, cwds(agents)...) {
		if k := volumeKey(p); k != "" && probe[k] == "" {
			probe[k] = p
		}
	}
	for k, p := range probe {
		if free, total, err := m.sys.DiskSpace(p); err == nil && total > 0 {
			out[k] = volReading{free, total}
		}
	}
	return out
}

func cwds(agents []model.Agent) []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.Cwd)
	}
	return out
}
