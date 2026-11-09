package resources

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blackcode-ch/tmx-go/internal/config"
	"github.com/blackcode-ch/tmx-go/internal/model"
	"github.com/blackcode-ch/tmx-go/internal/sysinfo"
)

func guardCfg() config.MemGuard {
	g := config.MemGuard{}
	config.NormalizeMemGuard(&g)
	return g
}

// machine builds a reading: free commit in GB and RAM available in percent of 16 GB.
func machine(commitGB, ramPct float64) sysinfo.Machine {
	return sysinfo.Machine{Cores: 8, MemTotal: 16 * GB, MemFree: uint64(ramPct / 100 * 16 * GB),
		CommitFree: uint64(commitGB * GB), CommitKnown: true}
}

func TestMemLevelThresholds(t *testing.T) {
	c := guardCfg()
	for _, tc := range []struct {
		commit, ram float64
		paging      bool
		want        Level
	}{
		{50, 60, false, LevelOK},
		{9.9, 60, false, LevelWarn},
		{50, 24, false, LevelWarn},
		{5.9, 60, false, LevelRefuse},
		{50, 14, false, LevelRefuse},
		{2.9, 60, false, LevelStop},
		{50, 9, false, LevelStop},
		{1.4, 60, false, LevelOrphan},
		{50, 4, false, LevelOrphan},
		{50, 19, true, LevelStop},    // sustained paging with RAM under 20%
		{50, 21, true, LevelWarn},    // RAM 21% is only under the warn line: paging adds nothing
		{50, 40, true, LevelOK},      // paging with RAM fine is not evidence of trouble
		{12, 14, false, LevelRefuse}, // the worse signal wins
	} {
		r := MemReading{CommitGB: tc.commit, CommitKnown: true, RAMPct: tc.ram, RAMKnown: true, PagingHot: tc.paging}
		if got := memLevel(r, c); got != tc.want {
			t.Errorf("commit %v ram %v paging %v: %s, want %s", tc.commit, tc.ram, tc.paging, got, tc.want)
		}
	}
	if got := memLevel(MemReading{}, c); got != LevelOK {
		t.Fatalf("no reading: %s", got)
	}
	off := c
	off.CommitStopGB, off.RAMStopPct = -1, -1
	if got := memLevel(MemReading{CommitGB: 2, CommitKnown: true, RAMPct: 9, RAMKnown: true}, off); got != LevelRefuse {
		t.Fatalf("stop thresholds off: %s", got)
	}
}

func TestWarnClearsOnlyWithMarginHeldFor300s(t *testing.T) {
	c := guardCfg()
	var g guardState
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	step := func(d time.Duration, commit, ram float64) Level {
		now = now.Add(d)
		return g.update(now, c, machine(commit, ram))
	}
	if got := step(0, 8, 60); got != LevelWarn {
		t.Fatalf("warn: %s", got)
	}
	// Above the warn line but inside the margin (RAM < 25+3 or commit < 10+2): stays warn.
	if got := step(time.Minute, 11, 60); got != LevelWarn {
		t.Fatalf("commit inside margin: %s", got)
	}
	if got := step(time.Minute, 50, 26); got != LevelWarn {
		t.Fatalf("RAM inside margin: %s", got)
	}
	// Both margins met: the 300 s clock starts, and a dip restarts it.
	if got := step(time.Minute, 12, 30); got != LevelWarn {
		t.Fatalf("clock starts: %s", got)
	}
	if got := step(4*time.Minute, 12, 30); got != LevelWarn {
		t.Fatalf("held 240 s: %s", got)
	}
	if got := step(10*time.Second, 11.5, 30); got != LevelWarn {
		t.Fatalf("dip: %s", got)
	}
	if got := step(time.Minute, 12, 30); got != LevelWarn {
		t.Fatalf("clock restarted: %s", got)
	}
	if got := step(299*time.Second, 12, 30); got != LevelWarn {
		t.Fatalf("299 s: %s", got)
	}
	if got := step(2*time.Second, 12, 30); got != LevelOK {
		t.Fatalf("held 300 s: %s", got)
	}
	// A level worse than warn, coming down, also passes through the warn latch.
	step(time.Second, 5, 60)
	if got := step(time.Second, 50, 60); got != LevelWarn || g.raw != LevelOK {
		t.Fatalf("recovering from refuse: %s raw %s", got, g.raw)
	}
}

func TestSustainedPagingNeedsTheHoldAndLowRAM(t *testing.T) {
	c := guardCfg()
	var g guardState
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	m := machine(50, 19)
	m.Pressure = sysinfo.Pressure{Kind: sysinfo.PressurePagesIn, Value: 5000}
	if got := g.update(now, c, m); got == LevelStop {
		t.Fatalf("not sustained yet: %s", got)
	}
	if got := g.update(now.Add(61*time.Second), c, m); got != LevelStop {
		t.Fatalf("sustained 61 s: %s", got)
	}
	m.Pressure.Value = 10 // cools down: the clock resets
	g.update(now.Add(70*time.Second), c, m)
	m.Pressure.Value = 5000
	if got := g.update(now.Add(80*time.Second), c, m); got == LevelStop {
		t.Fatalf("clock should have restarted: %s", got)
	}
}

func TestScaledFloorsForRAMBasedCommit(t *testing.T) {
	c := guardCfg()
	got := scaledMemGuard(c, 8, true)
	if got.CommitWarnGB != 1.6 || got.CommitRefuseGB != 0.96 || got.CommitStopGB != 0.48 || got.CommitOrphanGB != 0.24 {
		t.Fatalf("8 GB linux: %+v", got)
	}
	if got.RAMRefusePct != c.RAMRefusePct {
		t.Fatal("RAM percents never scale")
	}
	if w := scaledMemGuard(c, 8, false); w.CommitStopGB != c.CommitStopGB || w.CommitWarnGB != c.CommitWarnGB {
		t.Fatalf("windows scaled: %+v", w)
	}
	if big := scaledMemGuard(c, 64, true); big.CommitStopGB != 3 {
		t.Fatalf("a big machine keeps its floors: %+v", big)
	}
	set := c
	set.CommitStopGB = 4
	if got := scaledMemGuard(set, 8, true); got.CommitStopGB != 4 {
		t.Fatalf("a changed floor is never scaled: %+v", got)
	}
}

// The same injected reading must use its reader's commit model on every host.
func TestManagerCommitFloorsFollowReading(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ramBased bool
		commit   float64
		want     Level
	}{
		{"absolute stop", false, 2, LevelStop},
		{"absolute refuse", false, 5, LevelRefuse},
		{"RAM based warn", true, 2, LevelWarn},
		{"RAM based ok", true, 5, LevelOK},
		{"RAM based stop", true, 0.8, LevelStop},
		{"RAM based refuse", true, 1.5, LevelRefuse},
		{"darwin RAM based warn", true, 2, LevelWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := protectRig(t, config.Resources{})
			r.setMachine(tc.commit, 60)
			r.sys.mu.Lock()
			r.sys.mach.CommitRAMBased = tc.ramBased
			r.sys.mu.Unlock()
			r.step()
			if got := r.m.View().Protect.MemGuard.Level; got != tc.want {
				t.Fatalf("level %s, want %s", got, tc.want)
			}
		})
	}
}

func TestSelectVictimsOrderAndNeverStop(t *testing.T) {
	mk := func(id string, lvl model.Level, st model.AgentState) tree {
		return tree{agent: model.Agent{ID: id, Name: id, Level: lvl, State: st}}
	}
	trees := []tree{mk("head", model.LevelHead, model.StateIdle), mk("lead", model.LevelLead, model.StateWorking),
		mk("busy-big", model.LevelWorker, model.StateWorking), mk("idle-small", model.LevelWorker, model.StateIdle),
		mk("busy-small", model.LevelWorker, model.StateWorking), mk("idle-big", model.LevelWorker, model.StateIdle),
		mk("wa-watcher-h", model.LevelWorker, model.StateIdle), mk("paused", model.LevelWorker, model.StatePaused),
		mk("exempt", model.LevelWorker, model.StateIdle)}
	usage := make([]AgentUsage, len(trees))
	for i, t := range trees {
		usage[i].Mem = map[string]uint64{"busy-big": 900, "idle-small": 10, "busy-small": 100, "idle-big": 500}[t.agent.ID]
	}
	got := selectVictims(trees, usage, guardCfg(), map[string]int64{"exempt": 1 << 60}, 0)
	var ids []string
	for _, v := range got {
		ids = append(ids, v.id)
	}
	if want := []string{"idle-big", "idle-small", "busy-big", "busy-small", "lead"}; !slices.Equal(ids, want) {
		t.Fatalf("order %v, want %v", ids, want)
	}
}

// protectRig is a rig whose agents, workspace, home and temp folder share one volume.
func protectRig(t *testing.T, cfg config.Resources) *rig {
	t.Helper()
	cfg.DailyWriteGB = -1
	r := newRig(t, cfg)
	r.m.opts.Workspace, r.m.opts.TempDir = r.paths.Home, r.paths.Home
	for i := range r.fleet.agents {
		r.fleet.agents[i].Cwd = r.paths.Home
	}
	return r
}

func (r *rig) setMachine(commitGB, ramPct float64) {
	r.sys.mu.Lock()
	cpuBusy, cpuTotal := r.sys.mach.CPUBusy, r.sys.mach.CPUTotal
	r.sys.mach = machine(commitGB, ramPct)
	r.sys.mach.CPUBusy, r.sys.mach.CPUTotal = cpuBusy, cpuTotal
	r.sys.mu.Unlock()
}

func TestMemGuardPausesOneAtATimeByPriorityNeverTheHead(t *testing.T) {
	r := protectRig(t, config.Resources{})
	r.fleet.set("w2", func(a *model.Agent) { a.State = model.StateIdle }) // idle goes first
	r.setMachine(50, 60)
	r.step()
	if len(r.fleet.brakes) != 0 {
		t.Fatalf("braked at ok: %v", r.fleet.brakes)
	}
	r.setMachine(2, 60) // stop level by free commit
	r.step()
	if !slices.Equal(r.fleet.brakes, []string{"w2:memguard"}) {
		t.Fatalf("brakes: %v", r.fleet.brakes)
	}
	r.step() // inside restop_after_s: nothing more yet
	if len(r.fleet.brakes) != 1 {
		t.Fatalf("paced: %v", r.fleet.brakes)
	}
	r.now = r.now.Add(31 * time.Second)
	r.step()
	if !slices.Equal(r.fleet.brakes, []string{"w2:memguard", "w1:memguard"}) {
		t.Fatalf("second pause: %v", r.fleet.brakes)
	}
	r.now = r.now.Add(31 * time.Second)
	r.step() // only the head is left: it is critical
	if len(r.fleet.brakes) != 2 {
		t.Fatalf("the head was braked: %v", r.fleet.brakes)
	}
	v := r.m.View().Protect
	if v.MemGuard.Level != LevelStop || len(v.MemGuard.Stopped) != 2 {
		t.Fatalf("status: %+v", v.MemGuard)
	}
	if ok, why := r.m.Admit(0); ok || !strings.Contains(why, "low on memory") {
		t.Fatalf("admit at stop: %v %q", ok, why)
	}
	// The owner resumes one; it leaves the paused list.
	r.fleet.UnbrakeAgent("w2", BrakeMemGuard, "owner")
	r.m.Resumed("w2", BrakeMemGuard, false)
	r.setMachine(50, 60)
	r.step()
	if st := r.m.View().Protect.MemGuard; st.Level != LevelWarn || len(st.Stopped) != 1 {
		t.Fatalf("recovering: %+v", st) // warn is sticky for 300 s
	}
	if ok, _ := r.m.Admit(0); !ok {
		t.Fatal("warn must not refuse starts")
	}
}

func TestMemGuardRefuseBlocksStartsOnlyAndDisabledDoesNothing(t *testing.T) {
	r := protectRig(t, config.Resources{})
	r.setMachine(5, 60) // refuse
	r.step()
	if ok, why := r.m.Admit(0); ok || !strings.Contains(why, "no new agents") {
		t.Fatalf("admit at refuse: %v %q", ok, why)
	}
	if len(r.fleet.brakes) != 0 {
		t.Fatalf("refuse must not pause anyone: %v", r.fleet.brakes)
	}
	off := false
	r2 := protectRig(t, config.Resources{MemGuard: config.MemGuard{Enabled: &off}})
	r2.setMachine(1, 60)
	r2.step()
	if ok, _ := r2.m.Admit(0); !ok || len(r2.fleet.brakes) != 0 {
		t.Fatalf("disabled guard acted: %v", r2.fleet.brakes)
	}
}

func writeBoth(r *rig, w1, w2 uint64) {
	r.sys.add(201, 0, w1, 0) // w1's tool
	r.sys.add(300, 0, w2, 0)
}

func TestDiskReserveBrakesTopWriterBlocksStartsAndResumes(t *testing.T) {
	r := protectRig(t, config.Resources{})
	r.sys.free = 4 * GB        // under the 5 GB reserve
	r.step()                   // first tick: no write rate yet
	r.sys.free = 4*GB - 100*MB // and still falling
	writeBoth(r, 30*MB, 5*MB)
	r.step()
	if !slices.Equal(r.fleet.brakes, []string{"w1:disk_reserve"}) {
		t.Fatalf("brakes: %v", r.fleet.brakes)
	}
	if ok, why := r.m.Admit(0); ok || !strings.Contains(why, "free-space reserve") {
		t.Fatalf("admit under the reserve: %v %q", ok, why)
	}
	if ok, why := r.m.ReserveBlock(r.paths.Home, 1*GB); ok || why == "" {
		t.Fatalf("a copy under the reserve was allowed: %v %q", ok, why)
	}
	vol := r.m.View().Protect.DiskReserve
	if vol.State != VolLow || len(vol.Volumes) != 1 || vol.Volumes[0].ReserveGB != 5 || len(vol.Volumes[0].Braked) != 1 {
		t.Fatalf("status: %+v", vol)
	}
	// Still low and falling: the next writer goes, one per tick; w1 is not braked twice.
	r.sys.free = 3 * GB
	writeBoth(r, 30*MB, 5*MB)
	r.step()
	if !slices.Equal(r.fleet.brakes, []string{"w1:disk_reserve", "w2:disk_reserve"}) {
		t.Fatalf("second writer: %v", r.fleet.brakes)
	}
	// Owner resume works, and the agent is not re-braked inside the same episode.
	if !r.fleet.UnbrakeAgent("w1", BrakeDiskReserve, "owner") {
		t.Fatal("resume failed")
	}
	r.m.Resumed("w1", BrakeDiskReserve, false)
	r.sys.free = 2 * GB
	writeBoth(r, 30*MB, 0)
	r.step()
	if len(r.fleet.brakes) != 2 {
		t.Fatalf("braked again in one episode: %v", r.fleet.brakes)
	}
	// Room again: the episode ends, starts are allowed, a later low brakes again.
	r.sys.free = 50 * GB
	r.step()
	if ok, _ := r.m.Admit(0); !ok {
		t.Fatal("admit after recovery")
	}
	if ok, _ := r.m.ReserveBlock(r.paths.Home, 1*GB); !ok {
		t.Fatal("copy refused with room")
	}
	r.sys.free = 1 * GB
	writeBoth(r, 30*MB, 0)
	r.step()
	if len(r.fleet.brakes) != 3 || r.fleet.brakes[2] != "w1:disk_reserve" {
		t.Fatalf("new episode: %v", r.fleet.brakes)
	}
}

func eventKinds(r *rig) []string {
	var kinds []string
	for _, e := range r.m.View().Protect.Events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func TestDiskBrakeSkipsTheHeadAsTopWriter(t *testing.T) {
	r := protectRig(t, config.Resources{})
	r.sys.free = 4 * GB
	r.step()
	r.sys.free = 4*GB - 100*MB
	r.sys.add(100, 0, 500*MB, 0) // the head writes the most
	writeBoth(r, 30*MB, 5*MB)
	r.step()
	if !slices.Equal(r.fleet.brakes, []string{"w1:disk_reserve"}) {
		t.Fatalf("brakes: %v", r.fleet.brakes)
	}
}

func TestDiskBrakeOnlyExemptWritersAlertsAndPausesNobody(t *testing.T) {
	r := protectRig(t, config.Resources{})
	r.sys.free = 4 * GB
	r.step()
	r.sys.free = 4*GB - 100*MB
	r.sys.add(100, 0, 500*MB, 0) // only the head writes
	r.step()
	if len(r.fleet.brakes) != 0 {
		t.Fatalf("an exempt agent was paused: %v", r.fleet.brakes)
	}
	if !slices.Contains(eventKinds(r), "disk-reserve-exempt-writer") {
		t.Fatalf("no alert: %v", eventKinds(r))
	}
	if ok, _ := r.m.Admit(0); ok {
		t.Fatal("starts must stay blocked under the reserve")
	}
	if ok, _ := r.m.ReserveBlock(r.paths.Home, 1*GB); ok {
		t.Fatal("copies must stay blocked under the reserve")
	}
}

func TestDiskReserveObserveOnlyAlerts(t *testing.T) {
	r := protectRig(t, config.Resources{DiskReserve: config.DiskReserve{Mode: config.ReserveObserve}})
	r.sys.free = 1 * GB
	r.step()
	r.sys.free = 1*GB - 10*MB
	writeBoth(r, 30*MB, 5*MB)
	r.step()
	if len(r.fleet.brakes) != 0 {
		t.Fatalf("observe braked: %v", r.fleet.brakes)
	}
	if ok, _ := r.m.Admit(0); !ok {
		t.Fatal("observe must not block starts")
	}
	if ok, _ := r.m.ReserveBlock(r.paths.Home, 1*GB); !ok {
		t.Fatal("observe must not block copies")
	}
	var kinds []string
	for _, e := range r.m.View().Protect.Events {
		kinds = append(kinds, e.Kind)
	}
	if !slices.Contains(kinds, "disk-reserve-low") || !slices.Contains(kinds, "disk-reserve-would-brake") {
		t.Fatalf("events: %v", kinds)
	}
}

func TestDiskReserveOffDoesNothingAndWarnFiresEarly(t *testing.T) {
	r := protectRig(t, config.Resources{DiskReserve: config.DiskReserve{Mode: config.ReserveOff}})
	r.sys.free = 1 * GB
	r.step()
	writeBoth(r, 30*MB, 5*MB)
	r.step()
	if len(r.fleet.brakes) != 0 || len(r.m.View().Protect.DiskReserve.Volumes) != 0 {
		t.Fatalf("off acted: %v", r.fleet.brakes)
	}
	r2 := protectRig(t, config.Resources{})
	r2.sys.free = 8 * GB // between 5 and 10 GB: a warning, no brake
	r2.step()
	writeBoth(r2, 30*MB, 0)
	r2.step()
	if r2.m.View().Protect.DiskReserve.State != VolWarn || len(r2.fleet.brakes) != 0 {
		t.Fatalf("warn: %+v brakes %v", r2.m.View().Protect.DiskReserve, r2.fleet.brakes)
	}
	if h := r2.m.Health(); h.DiskReserve.State != VolWarn || h.DiskReserve.Mode != config.ReserveOn || h.MemGuard.Level != LevelOK {
		t.Fatalf("health: %+v", h)
	}
}

func TestDefaultsMatchPython(t *testing.T) {
	c := config.DefaultResources()
	g, d := c.MemGuard, c.DiskReserve
	if g.CommitWarnGB != 10 || g.RAMWarnPct != 25 || g.CommitRefuseGB != 6 || g.RAMRefusePct != 15 || g.CommitStopGB != 3 ||
		g.RAMStopPct != 10 || g.CommitOrphanGB != 1.5 || g.RAMOrphanPct != 5 || g.PagingPagesInS != 1000 || g.PagingSustainS != 60 ||
		g.PSIAvg10Pct != 10 || g.HysteresisS != 120 || g.RestopAfterS != 30 || !g.On() ||
		g.WarnClearPctMargin != 3 || g.WarnClearGBMargin != 2 || g.WarnClearHoldS != 300 {
		t.Fatalf("memguard defaults: %+v", g)
	}
	if d.Mode != "on" || d.GB != 5 || d.Pct != 0 || d.WarnFactor != 2 {
		t.Fatalf("disk_reserve defaults: %+v", d)
	}
	if err := (config.Resources{DiskReserve: config.DiskReserve{Mode: "sometimes"}}).Validate(); err == nil {
		t.Fatal("bad mode accepted")
	}
}
