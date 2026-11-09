package resources

import (
	"errors"
	"fmt"
	"time"

	"github.com/blackcode-ch/tmx-go/internal/model"
	"github.com/blackcode-ch/tmx-go/internal/sysinfo"
)

type tree struct {
	agent model.Agent
	procs []sysinfo.Proc
	use   []sysinfo.Usage
	ok    []bool
}

type action struct {
	brake          bool // else unbrake
	id, name       string
	reason, detail string
	ask            bool
	onDone         func(paused bool) // a brake: told whether the agent was really paused (outside the lock)
}

// Tick takes one sample, applies the brakes, pumps the launch queue and publishes the
// sample. Run calls it every SampleSecs; tests call it directly.
func (m *Manager) Tick() {
	if m.fleet == nil {
		return
	}
	t0 := time.Now()
	now := m.opts.Now()
	agents := m.fleet.LiveAgents()
	running, queued, paused := m.fleet.Counts()

	// The OS reads happen outside the lock: one process table, then only the agents' trees.
	var procs []sysinfo.Proc
	if len(agents) > 0 {
		procs, _ = m.sys.Procs()
	}
	trees := make([]tree, len(agents))
	for i, a := range agents {
		t := tree{agent: a, procs: sysinfo.Tree(procs, a.PID)}
		t.use, t.ok = make([]sysinfo.Usage, len(t.procs)), make([]bool, len(t.procs))
		for j, p := range t.procs {
			t.use[j], t.ok[j] = m.sys.ProcUsage(p.PID)
		}
		trees[i] = t
	}
	mach, merr := m.sys.Machine() // partial readings remain visible but cannot admit background jobs
	disks, derr := m.sys.Disks()
	// Disk I/O this system cannot measure: the disk-saturated brake stays off (DiskQueue -1)
	// while every other brake and admission work. A real read error remains fail-safe.
	unsupported := errors.Is(derr, sysinfo.ErrUnsupported)
	if unsupported {
		derr = nil
	}
	path := m.opts.Workspace
	if path == "" {
		path = "."
	}
	dfree, dtotal, _ := m.sys.DiskSpace(path)
	vols := m.readVolumes(path, agents)
	healthy := merr == nil && derr == nil
	m.staleAfter.Store(int64(3 * m.interval()))
	if healthy {
		m.lastCompleted.Store(time.Now().UnixNano())
	} else {
		m.lastCompleted.Store(0)
	}

	m.mu.Lock()
	m.clearWatchdogRestorationsLocked()
	m.prot.read = vols
	acts := m.evaluateLocked(now, trees, mach, disks, derr == nil && !unsupported)
	var dropped []action
	if !healthy {
		kept := acts[:0]
		for _, a := range acts {
			if !a.brake {
				kept = append(kept, a)
			} else {
				delete(m.diskTurns, a.id)
				dropped = append(dropped, a)
			}
		}
		acts = kept
	}
	guardEvents := m.prot.pending
	m.prot.pending = nil
	mu := &m.sample.Machine
	mu.DiskFree, mu.DiskTotal, mu.DiskPath = dfree, dtotal, path
	mu.Running, mu.Queued, mu.Paused = running, queued, paused
	sample := cloneSample(m.sample)
	m.mu.Unlock()

	for _, a := range dropped {
		if a.onDone != nil {
			a.onDone(false)
		}
	}
	for _, e := range guardEvents {
		m.log.Warn("guard", "kind", e.Kind, "agent", e.Agent, "volume", e.Volume, "detail", e.Detail)
		m.fleet.Publish(model.BusEvent{Type: model.EvResourcesGuard, Data: e})
	}
	m.apply(acts)
	m.fleet.PumpQueue()
	m.fleet.Publish(model.BusEvent{Type: model.EvResourcesSampled, Data: sample})
	m.save(false)
	m.account(time.Since(t0))
	m.staleAfter.Store(int64(3 * m.interval()))
	if merr == nil && derr == nil {
		m.lastCompleted.Store(time.Now().UnixNano())
	} else {
		m.lastCompleted.Store(0)
	}
}

// evaluateLocked turns the readings into the new sample, the day's accounting, the history
// and the brake actions to take.
func (m *Manager) evaluateLocked(now time.Time, trees []tree, mach sysinfo.Machine, disks []sysinfo.Disk, disksOK bool) []action {
	var acts []action
	c := m.cfg
	nowMs := now.UnixMilli()
	if day := now.Format(time.DateOnly); m.led.Day != day {
		for _, t := range trees { // a new day's budget resumes everyone it paused
			if t.agent.State == model.StatePaused && t.agent.PausedReason == BrakeWriteBudget {
				acts = append(acts, action{id: t.agent.ID, name: t.agent.Name, reason: BrakeWriteBudget,
					detail: "resumed: a new day, a new write budget"})
			}
		}
		m.led.newDay(day)
		m.dirty = true
	}
	first := m.prevAt.IsZero()
	dt := now.Sub(m.prevAt).Seconds()
	if first || dt <= 0 {
		dt = 0
	}

	// Agents.
	next := make(map[int]procPrev, len(m.prev))
	usage := make([]AgentUsage, 0, len(trees))
	live := map[string]model.Agent{}
	var agentRate float64
	for _, t := range trees {
		a := t.agent
		live[a.ID] = a
		u := AgentUsage{AgentID: a.ID}
		var cpu time.Duration
		var written uint64
		for j, p := range t.procs {
			if !t.ok[j] {
				continue
			}
			us := t.use[j]
			u.Mem += us.Mem
			u.Procs++
			if pp, seen := m.prev[p.PID]; seen && pp.start == p.Start {
				if us.CPU >= pp.cpu {
					cpu += us.CPU - pp.cpu
				}
				if us.Written >= pp.written {
					written += us.Written - pp.written
				}
			} else {
				// A process seen for the first time started inside the agent: everything
				// it wrote is the agent's. Its CPU counts from the next sample on.
				written += us.Written
			}
			next[p.PID] = procPrev{start: p.Start, cpu: us.CPU, written: us.Written}
		}
		if dt > 0 {
			u.CPU = cpu.Seconds() / dt * 100
			u.WriteRate = float64(written) / dt
		}
		if written > 0 {
			m.led.Written[a.ID] += written
			m.dirty = true
		}
		agentRate += u.WriteRate
		u.WrittenToday = m.led.Written[a.ID]
		u.Budget = m.budgetLocked(a.ID)
		if until := c.Exempt[a.Name]; until > nowMs {
			u.ExemptUntil = until
		}
		usage = append(usage, u)
	}
	m.prev = next
	var total uint64
	for id, w := range m.led.Written {
		total += w
		if _, ok := live[id]; !ok && w > 0 {
			usage = append(usage, AgentUsage{AgentID: id, WrittenToday: w, Budget: m.budgetLocked(id)})
		}
	}

	// Machine.
	mu := MachineUsage{Cores: mach.Cores, MemTotal: mach.MemTotal, MemFree: mach.MemFree,
		DiskQueue: -1, DiskWriteRate: -1, AgentWriteRate: agentRate, WrittenToday: total}
	if p := m.prevMachine; p.CPUTotal > 0 && mach.CPUTotal > p.CPUTotal && mach.CPUBusy >= p.CPUBusy {
		mu.CPU = float64(mach.CPUBusy-p.CPUBusy) / float64(mach.CPUTotal-p.CPUTotal) * 100
	}
	if mach.CPUTotal > 0 {
		m.prevMachine = mach
	}
	if disksOK {
		nd := make(map[string]sysinfo.Disk, len(disks))
		if dt > 0 {
			mu.DiskQueue, mu.DiskWriteRate = 0, 0
		}
		for _, d := range disks {
			nd[d.Name] = d
			p, ok := m.prevDisks[d.Name]
			if !ok || dt == 0 || d.IOTime < p.IOTime || d.Written < p.Written {
				continue
			}
			mu.DiskQueue = max(mu.DiskQueue, (d.IOTime-p.IOTime).Seconds()/dt)
			mu.DiskWriteRate += float64(d.Written-p.Written) / dt
		}
		m.prevDisks = nd
	}
	hot := c.DiskQueue > 0 && mu.DiskQueue >= c.DiskQueue && agentRate >= c.DiskAgentMBs*MB
	switch {
	case !hot:
		m.satSince = time.Time{}
	case m.satSince.IsZero():
		m.satSince = now
	}
	mu.Saturated = hot

	// Brakes.
	acts = append(acts, m.brakesLocked(now, trees, usage)...)
	for id := range m.led.Paused {
		if a, ok := live[id]; !ok || a.State != model.StatePaused {
			delete(m.led.Paused, id)
			m.dirty = true
		}
	}

	taken := map[string]bool{}
	for _, a := range acts {
		if a.brake {
			taken[a.id] = true
		}
	}
	for id := range m.prot.braked { // forget what the owner resumed or what stopped by itself
		if a, ok := live[id]; !ok || a.State != model.StatePaused {
			delete(m.prot.braked, id)
		}
	}
	acts = append(acts, m.protectLocked(now, mach, trees, usage, taken)...)

	// History and the sample.
	m.history.Machine = appendPoint(m.history.Machine, Point{T: nowMs, CPU: float32(mu.CPU), Mem: mu.MemFree, W: float32(max(mu.DiskWriteRate, 0))})
	for _, u := range usage[:len(trees)] {
		m.history.Agents[u.AgentID] = appendPoint(m.history.Agents[u.AgentID], Point{T: nowMs, CPU: float32(u.CPU), Mem: u.Mem, W: float32(u.WriteRate)})
	}
	for id := range m.history.Agents {
		if _, ok := live[id]; !ok {
			delete(m.history.Agents, id)
		}
	}
	m.sample = Sample{At: nowMs, Interval: c.SampleSecs, Machine: mu, Agents: usage}
	m.sampled, m.prevAt, m.admitted = now, now, 0
	return acts
}

func appendPoint(h []Point, p Point) []Point {
	h = append(h, p)
	if over := len(h) - HistoryLen; over > 0 {
		h = append(h[:0], h[over:]...)
	}
	return h
}

// budgetLocked is what an agent may write today before it is paused (0 = no budget).
func (m *Manager) budgetLocked(id string) uint64 {
	if m.cfg.DailyWriteGB <= 0 {
		return 0
	}
	b := uint64(m.cfg.DailyWriteGB*GB) + m.led.Extra[id]
	return max(b, m.led.Rearm[id])
}

// brakesLocked decides which agents to pause (budget, memory, the saturated disk's heaviest
// writer) and which disk turns end. usage[i] belongs to trees[i].
func (m *Manager) brakesLocked(now time.Time, trees []tree, usage []AgentUsage) []action {
	var acts []action
	c := m.cfg
	nowMs := now.UnixMilli()
	for id, until := range m.diskTurns {
		if now.After(until) || now.Equal(until) {
			delete(m.diskTurns, id)
			acts = append(acts, action{id: id, reason: BrakeDisk, detail: "resumed: its turn off the saturated disk is over"})
		}
	}
	heaviest := -1
	for i, t := range trees {
		a, u := t.agent, usage[i]
		if a.State == model.StatePaused && a.PausedAt > 0 && nowMs-a.PausedAt >= sysinfo.MaxPause.Milliseconds() {
			delete(m.diskTurns, a.ID)
			acts = append(acts, action{id: a.ID, detail: "resumed: the maximum safe pause duration expired"})
			continue
		}
		if a.Harness == model.HarnessShell || a.State == model.StatePaused || c.Exempt[a.Name] > nowMs {
			continue
		}
		who := a.Name
		if a.Level == model.LevelHead {
			who = "The head"
		}
		if b := u.Budget; b > 0 && u.WrittenToday >= b {
			acts = append(acts, action{brake: true, id: a.ID, name: a.Name, reason: BrakeWriteBudget, ask: true,
				detail: fmt.Sprintf("%s wrote %s today; its daily write budget is %s.", who, Bytes(int64(u.WrittenToday)), Bytes(int64(b)))})
			continue
		}
		if cap := uint64(c.MemoryCapMB) * MB; cap > 0 && u.Mem > cap && !m.led.MemOK[a.ID] {
			acts = append(acts, action{brake: true, id: a.ID, name: a.Name, reason: BrakeMemory, ask: true,
				detail: fmt.Sprintf("%s uses %s of memory; the cap per agent is %s.", who, Bytes(int64(u.Mem)), Bytes(int64(cap)))})
			continue
		}
		if a.Level != model.LevelHead && u.WriteRate >= minWriterRate && (heaviest < 0 || u.WriteRate > usage[heaviest].WriteRate) {
			heaviest = i
		}
	}
	sat := !m.satSince.IsZero() && now.Sub(m.satSince) >= time.Duration(c.DiskBusySecs)*time.Second
	if sat && len(m.diskTurns) == 0 && heaviest >= 0 {
		a, u := trees[heaviest].agent, usage[heaviest]
		m.diskTurns[a.ID] = now.Add(time.Duration(c.SaturationPauseSecs) * time.Second)
		m.satSince = now // the disk gets a full window to recover before the next turn
		acts = append(acts, action{brake: true, id: a.ID, name: a.Name, reason: BrakeDisk,
			detail: fmt.Sprintf("The disk is saturated and %s writes the most (%s/s); paused for %d s so the machine stays responsive.",
				a.Name, Bytes(int64(u.WriteRate)), c.SaturationPauseSecs)})
	}
	return acts
}

func (m *Manager) apply(acts []action) {
	for _, a := range acts {
		if !a.brake {
			if m.fleet.UnbrakeAgent(a.id, a.reason, a.detail) {
				m.mu.Lock()
				delete(m.led.Paused, a.id)
				m.dirty = true
				m.mu.Unlock()
			}
			continue
		}
		procs, err := m.fleet.BrakeAgent(a.id, a.reason, a.detail, a.ask)
		m.mu.Lock()
		if err != nil {
			if a.reason == BrakeDisk {
				delete(m.diskTurns, a.id)
			}
			m.mu.Unlock()
			m.log.Warn("brake", "agent", a.name, "reason", a.reason, "err", err)
			if a.onDone != nil {
				a.onDone(false)
			}
			continue
		}
		m.led.Paused[a.id] = procs
		m.dirty = true
		m.mu.Unlock()
		if a.onDone != nil {
			a.onDone(true)
		}
		m.save(true) // a crash must not leave these processes paused for good
	}
}

func (m *Manager) account(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond) // keep sub-microsecond ticks nonzero
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cost.Ticks++
	m.totalMs += ms
	m.cost.AvgMs = m.totalMs / float64(m.cost.Ticks)
	m.cost.MaxMs = max(m.cost.MaxMs, ms)
	m.cost.CPUPct = m.cost.AvgMs / (float64(m.cfg.SampleSecs) * 1000) * 100
}
