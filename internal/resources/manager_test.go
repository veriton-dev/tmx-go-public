package resources

import (
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackcode-ch/tmx-go/internal/config"
	"github.com/blackcode-ch/tmx-go/internal/model"
	"github.com/blackcode-ch/tmx-go/internal/slots"
	"github.com/blackcode-ch/tmx-go/internal/sysinfo"
)

// fakeSys: a process table and counters the test moves forward.
type fakeSys struct {
	mu      sync.Mutex
	procs   []sysinfo.Proc
	use     map[int]sysinfo.Usage
	mach    sysinfo.Machine
	disk    sysinfo.Disk
	resumed [][]sysinfo.Proc
	free    uint64 // DiskSpace free bytes; 0 = 100 GB
}

func (s *fakeSys) Procs() ([]sysinfo.Proc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sysinfo.Proc(nil), s.procs...), nil
}
func (s *fakeSys) ProcUsage(pid int) (sysinfo.Usage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.use[pid]
	return u, ok
}
func (s *fakeSys) Machine() (sysinfo.Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mach, nil
}
func (s *fakeSys) Disks() ([]sysinfo.Disk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []sysinfo.Disk{s.disk}, nil
}
func (s *fakeSys) DiskSpace(string) (uint64, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.free > 0 {
		return s.free, 500 * GB, nil
	}
	return 100 * GB, 500 * GB, nil
}
func (s *fakeSys) ResumeProcs(l []sysinfo.Proc) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumed = append(s.resumed, l)
	return len(l)
}

// add moves a process's counters forward.
func (s *fakeSys) add(pid int, cpu time.Duration, written uint64, mem uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.use[pid]
	u.CPU += cpu
	u.Written += written
	if mem > 0 {
		u.Mem = mem
	}
	s.use[pid] = u
}

type fakeFleet struct {
	mu       sync.Mutex
	agents   []model.Agent
	brakes   []string // "name:reason"
	unbrakes []string
	pumps    int
	events   int
}

func (f *fakeFleet) LiveAgents() []model.Agent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.Agent(nil), f.agents...)
}
func (f *fakeFleet) Counts() (int, int, int) { return len(f.agents), 0, 0 }
func (f *fakeFleet) set(id string, fn func(*model.Agent)) {
	for i := range f.agents {
		if f.agents[i].ID == id {
			fn(&f.agents[i])
		}
	}
}
func (f *fakeFleet) BrakeAgent(id, reason, detail string, ask bool) ([]sysinfo.Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set(id, func(a *model.Agent) {
		a.State, a.PausedReason = model.StatePaused, reason
		f.brakes = append(f.brakes, a.Name+":"+reason)
	})
	return []sysinfo.Proc{{PID: 100, Start: 7}}, nil
}
func (f *fakeFleet) UnbrakeAgent(id, reason, why string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := false
	f.set(id, func(a *model.Agent) {
		if a.State == model.StatePaused && a.PausedReason == reason {
			a.State, a.PausedReason, ok = model.StateWorking, "", true
			f.unbrakes = append(f.unbrakes, a.Name+":"+reason)
		}
	})
	return ok
}
func (f *fakeFleet) PumpQueue()             { f.mu.Lock(); f.pumps++; f.mu.Unlock() }
func (f *fakeFleet) Publish(model.BusEvent) { f.mu.Lock(); f.events++; f.mu.Unlock() }

type rig struct {
	m     *Manager
	sys   *fakeSys
	fleet *fakeFleet
	now   time.Time
	paths config.Paths
}

func newRig(t *testing.T, cfg config.Resources) *rig {
	t.Helper()
	r := &rig{now: time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local), paths: config.Paths{Home: t.TempDir()}}
	r.sys = &fakeSys{
		procs: []sysinfo.Proc{
			{PID: os.Getpid(), PPID: 1, PGID: 1},
			{PID: 100, PPID: os.Getpid(), PGID: 100, Start: 7},  // head
			{PID: 200, PPID: os.Getpid(), PGID: 200, Start: 8},  // worker w1
			{PID: 201, PPID: 200, PGID: 201, Start: 9},          // its Bash tool, own group
			{PID: 300, PPID: os.Getpid(), PGID: 300, Start: 10}, // worker w2
		},
		use:  map[int]sysinfo.Usage{100: {Mem: 300 * MB}, 200: {Mem: 400 * MB}, 201: {Mem: 100 * MB}, 300: {Mem: 200 * MB}},
		mach: sysinfo.Machine{Cores: 8, MemTotal: 16 * GB, MemFree: 8 * GB},
	}
	r.fleet = &fakeFleet{agents: []model.Agent{
		{ID: "h", Name: "head", Level: model.LevelHead, PID: 100, State: model.StateIdle},
		{ID: "w1", Name: "w1", Level: model.LevelWorker, PID: 200, State: model.StateWorking},
		{ID: "w2", Name: "w2", Level: model.LevelWorker, PID: 300, State: model.StateWorking},
	}}
	r.m = New(Options{Paths: r.paths, Config: cfg, Sys: r.sys, Now: func() time.Time { return r.now }})
	r.m.Attach(r.fleet)
	if err := r.m.Start(); err != nil {
		t.Fatal(err)
	}
	return r
}

// step advances the clock and the machine by one second and ticks.
func (r *rig) step() {
	r.now = r.now.Add(time.Second)
	r.sys.mu.Lock()
	r.sys.mach.CPUTotal += 8 * time.Second
	r.sys.mach.CPUBusy += 2 * time.Second
	r.sys.mu.Unlock()
	r.m.Tick()
}

func usageOf(t *testing.T, s Sample, id string) AgentUsage {
	t.Helper()
	for _, u := range s.Agents {
		if u.AgentID == id {
			return u
		}
	}
	t.Fatalf("no usage for %s in %+v", id, s.Agents)
	return AgentUsage{}
}

func TestSampleMeasuresTrees(t *testing.T) {
	r := newRig(t, config.Resources{DailyWriteGB: -1})
	r.step()
	r.sys.add(200, 300*time.Millisecond, 10*MB, 0)
	r.sys.add(201, 200*time.Millisecond, 5*MB, 0) // the tool in its own group counts too
	r.step()
	s := r.m.View().Sample
	u := usageOf(t, s, "w1")
	if u.Procs != 2 || u.Mem != 500*MB || int(u.CPU+0.5) != 50 || u.WriteRate != 15*MB || u.WrittenToday != 15*MB {
		t.Fatalf("w1 usage: %+v", u)
	}
	if s.Machine.CPU != 25 || s.Machine.MemFree != 8*GB || s.Machine.DiskFree != 100*GB || s.Machine.WrittenToday != 15*MB {
		t.Fatalf("machine: %+v", s.Machine)
	}
	if h := r.m.View().History.Agents["w1"]; len(h) != 2 || h[1].CPU < 49 {
		t.Fatalf("history: %+v", h)
	}
	if r.fleet.events != 2 || r.fleet.pumps != 2 {
		t.Fatalf("events %d pumps %d", r.fleet.events, r.fleet.pumps)
	}
	if v := r.m.View().Cost; v.Ticks != 2 || v.AvgMs < 0 || (runtime.GOOS != "windows" && v.AvgMs == 0) {
		// Windows' clock can be coarser than one tick, so a fast tick may measure as 0.
		t.Fatalf("cost: %+v", v)
	}
}

func TestWriteBudgetBrakeRearmRaiseAndNewDay(t *testing.T) {
	r := newRig(t, config.Resources{DailyWriteGB: 0.01, RearmGB: 0.005}) // ~10 MB, rearm ~5 MB
	r.step()
	r.sys.add(201, 0, 6*MB, 0)
	r.step()
	if len(r.fleet.brakes) != 0 {
		t.Fatal("under budget")
	}
	r.sys.add(201, 0, 6*MB, 0)
	r.step()
	if len(r.fleet.brakes) != 1 || r.fleet.brakes[0] != "w1:"+BrakeWriteBudget {
		t.Fatalf("brakes %v", r.fleet.brakes)
	}
	b, _ := os.ReadFile(ledgerPath(r.paths))
	if !strings.Contains(string(b), `"paused":{"w1"`) {
		t.Fatalf("paused processes not saved: %s", b)
	}
	// Resume: braked again only after RearmGB more.
	r.fleet.UnbrakeAgent("w1", BrakeWriteBudget, "")
	r.m.Resumed("w1", BrakeWriteBudget, false)
	r.sys.add(201, 0, 1*MB, 0)
	r.step()
	if len(r.fleet.brakes) != 1 {
		t.Fatal("rearmed too early")
	}
	r.sys.add(201, 0, 6*MB, 0)
	r.step()
	if len(r.fleet.brakes) != 2 {
		t.Fatalf("not braked again after the rearm: %v", r.fleet.brakes)
	}
	// Raise: another full budget.
	r.fleet.UnbrakeAgent("w1", BrakeWriteBudget, "")
	r.m.Resumed("w1", BrakeWriteBudget, true)
	r.step()
	if u := usageOf(t, r.m.View().Sample, "w1"); u.Budget < 20*MB {
		t.Fatalf("raised budget %d", u.Budget)
	}
	// A new day resumes and resets.
	r.sys.add(201, 0, 30*MB, 0)
	r.step()
	if r.fleet.agents[1].State != model.StatePaused {
		t.Fatal("over the raised budget")
	}
	r.now = r.now.Add(24 * time.Hour)
	r.step()
	if len(r.fleet.unbrakes) != 3 || r.fleet.agents[1].State == model.StatePaused {
		t.Fatalf("new day did not resume: %v", r.fleet.unbrakes)
	}
	if u := usageOf(t, r.m.View().Sample, "w1"); u.WrittenToday != 0 || u.Budget != budgetOf(0.01) {
		t.Fatalf("new day accounting: %+v", u)
	}
}

func TestExemptAndMemoryCap(t *testing.T) {
	r := newRig(t, config.Resources{DailyWriteGB: 0.001, MemoryCapMB: 450})
	if _, err := r.m.SetExempt("w1", r.now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	r.step()
	r.sys.add(200, 0, 5*MB, 0)
	r.sys.add(300, 0, 0, 900*MB)
	r.step()
	if len(r.fleet.brakes) != 1 || r.fleet.brakes[0] != "w2:"+BrakeMemory {
		t.Fatalf("exempt w1 must not brake, w2 over its memory cap must: %v", r.fleet.brakes)
	}
	r.fleet.UnbrakeAgent("w2", BrakeMemory, "")
	r.m.Resumed("w2", BrakeMemory, false)
	r.step()
	if len(r.fleet.brakes) != 1 {
		t.Fatal("a waived memory cap must not brake again today")
	}
	// The exemption expires.
	r.now = r.now.Add(2 * time.Hour)
	r.sys.add(200, 0, 1*MB, 0)
	r.step()
	if len(r.fleet.brakes) != 2 || r.fleet.brakes[1] != "w1:"+BrakeWriteBudget {
		t.Fatalf("after the exemption: %v", r.fleet.brakes)
	}
}

func TestSaturatedDiskPausesHeaviestWriterForATurn(t *testing.T) {
	r := newRig(t, config.Resources{DailyWriteGB: -1, DiskQueue: 8, DiskBusySecs: 2, DiskAgentMBs: 50, SaturationPauseSecs: 3})
	r.step()
	for i := 0; i < 3; i++ {
		r.sys.mu.Lock()
		r.sys.disk.IOTime += 10 * time.Second // 10 requests in flight on average
		r.sys.disk.Written += 200 * MB
		r.sys.mu.Unlock()
		r.sys.add(201, 0, 120*MB, 0) // w1 writes the most
		r.sys.add(300, 0, 30*MB, 0)
		r.sys.add(100, 0, 500*MB, 0) // the head writes even more, but is never the one
		r.step()
	}
	s := r.m.View().Sample.Machine
	if !s.Saturated || s.DiskQueue < 9.9 {
		t.Fatalf("machine: %+v", s)
	}
	if len(r.fleet.brakes) != 1 || r.fleet.brakes[0] != "w1:"+BrakeDisk {
		t.Fatalf("brakes %v", r.fleet.brakes)
	}
	for i := 0; i < 3; i++ {
		r.step()
	}
	if len(r.fleet.unbrakes) != 1 || r.fleet.unbrakes[0] != "w1:"+BrakeDisk {
		t.Fatalf("the turn did not end: %v", r.fleet.unbrakes)
	}
}

func TestAdmit(t *testing.T) {
	r := newRig(t, config.Resources{MaxAgents: 3, MinFreeRAMMB: 1500})
	if ok, _ := r.m.Admit(2); !ok {
		t.Fatal("2 of 3 admits")
	}
	if ok, why := r.m.Admit(3); ok || why != "3 of 3 agents running" {
		t.Fatalf("3 of 3: %v %q", ok, why)
	}
	r.sys.mach.MemTotal, r.sys.mach.MemFree = 8*GB, 1800*MB // 22% RAM: memguard only warns
	r.step()
	if ok, _ := r.m.Admit(0); !ok {
		t.Fatal("1.8 GB free admits one")
	}
	if ok, why := r.m.Admit(0); ok || !strings.Contains(why, "below the 1.5 GB minimum") {
		t.Fatalf("a second start in the same sample: %v %q", ok, why)
	}
	r.step()
	if ok, _ := r.m.Admit(0); !ok {
		t.Fatal("a new sample resets the reservation")
	}
	if !r.m.LowPriority() {
		t.Fatal("low priority by default")
	}
}

func TestStartContinuesLeftoverPausedProcesses(t *testing.T) {
	home := config.Paths{Home: t.TempDir()}
	led := ledger{Day: "2026-10-21", Paused: map[string][]sysinfo.Proc{"w1": {{PID: 4242, Start: 99}}}}.fill()
	if err := led.save(home); err != nil {
		t.Fatal(err)
	}
	sys := &fakeSys{use: map[int]sysinfo.Usage{}}
	m := New(Options{Paths: home, Sys: sys})
	m.Attach(&fakeFleet{})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if len(sys.resumed) != 1 || sys.resumed[0][0].PID != 4242 {
		t.Fatalf("resumed %v", sys.resumed)
	}
	b, _ := os.ReadFile(ledgerPath(home))
	if strings.Contains(string(b), "4242") {
		t.Fatalf("leftovers must be forgotten after continuing them: %s", b)
	}
}

func TestSetConfigResizesSlots(t *testing.T) {
	sl := slots.New(slots.Options{})
	var saved config.Resources
	m := New(Options{Paths: config.Paths{Home: t.TempDir()}, Slots: sl, Save: func(c config.Resources) error { saved = c; return nil }})
	if v := sl.View(); len(v.Kinds) != 2 || v.Kinds[0].Size != 2 {
		t.Fatalf("default slots %+v", v.Kinds)
	}
	c := m.Config()
	c.Slots["heavy"] = 4
	c.Priority = "normal"
	if _, err := m.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	if saved.Slots["heavy"] != 4 || m.LowPriority() || sl.View().Kinds[1].Size != 4 {
		t.Fatalf("config not applied: %+v", saved)
	}
	c.Priority = "fast"
	if _, err := m.SetConfig(c); err == nil {
		t.Fatal("invalid priority accepted")
	}
}

func budgetOf(gb float64) uint64 { return uint64(gb * GB) }

func TestPauseLeaseExpiryClearsBrake(t *testing.T) {
	now := time.Unix(1000, 0)
	m := New(Options{})
	a := model.Agent{ID: "paused", Name: "paused", State: model.StatePaused, PausedAt: now.Add(-sysinfo.MaxPause).UnixMilli()}
	acts := m.brakesLocked(now, []tree{{agent: a}}, []AgentUsage{{}})
	if len(acts) != 1 || acts[0].brake || acts[0].id != a.ID {
		t.Fatalf("expired pause remains in UI: %+v", acts)
	}
}
