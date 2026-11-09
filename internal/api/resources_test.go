package api_test

import (
	"testing"

	"github.com/blackcode-ch/tmx-go/internal/api/apitest"
	"github.com/blackcode-ch/tmx-go/internal/config"
	"github.com/blackcode-ch/tmx-go/internal/model"
	"github.com/blackcode-ch/tmx-go/internal/resources"
	"github.com/blackcode-ch/tmx-go/internal/slots"
)

func TestResourcesConfigAndQueue(t *testing.T) {
	d := apitest.New(t)
	owner := client{t: t, base: d.URL, token: d.OwnerToken}
	var v resources.View
	if owner.do("GET", "/api/resources", nil, &v) != 200 || v.Config.MaxAgents != 8 || v.Config.Slots["heavy"] != 2 {
		t.Fatalf("view: %+v", v.Config)
	}
	cfg := v.Config
	cfg.MaxAgents = 2
	var saved config.Resources
	if owner.do("PUT", "/api/resources/config", cfg, &saved) != 200 || saved.MaxAgents != 2 {
		t.Fatalf("put config: %+v", saved)
	}
	bad := cfg
	bad.Priority = "turbo"
	if s := owner.do("PUT", "/api/resources/config", bad, nil); s != 400 {
		t.Fatalf("bad config: %d", s)
	}

	project := workerProject(t, d)
	var w1, w2 model.Agent
	owner.do("POST", "/api/agents", map[string]any{"name": "w1", "project_id": project}, &w1)
	owner.do("POST", "/api/agents", map[string]any{"name": "w2", "project_id": project, "prompt": "go"}, &w2)
	if w1.State == model.StateQueued || w2.State != model.StateQueued || w2.QueuePos != 1 || w2.QueueReason != "2 of 2 agents running" {
		t.Fatalf("w1 %s, w2 %s #%d %q", w1.State, w2.State, w2.QueuePos, w2.QueueReason)
	}
	if s := owner.do("POST", "/api/agents", map[string]any{"name": "w3", "project_id": project, "no_wait": true}, nil); s != 429 {
		t.Fatalf("no_wait launch: %d", s)
	}
	// An agent cannot jump the queue; the owner can.
	lead := client{t: t, base: d.URL, token: d.AgentToken(t, "head")}
	if s := lead.do("POST", "/api/agents/w2/start?force=1", nil, nil); s != 403 {
		t.Fatalf("agent force: %d", s)
	}
	var a model.Agent
	if owner.do("POST", "/api/agents/w2/start?force=1", nil, &a) != 200 || a.State == model.StateQueued {
		t.Fatalf("start anyway: %+v", a)
	}
	if s := owner.do("POST", "/api/agents/w2/resume", nil, nil); s != 409 {
		t.Fatalf("resume of a running agent: %d", s)
	}
	if owner.do("POST", "/api/resources/exempt", map[string]any{"agent": "w1", "hours": 2}, &saved) != 200 || saved.Exempt["w1"] == 0 {
		t.Fatalf("exempt: %+v", saved.Exempt)
	}
	if s := lead.do("PUT", "/api/resources/config", cfg, nil); s != 403 {
		t.Fatalf("agent put config: %d", s)
	}
}

func TestSlotsAPI(t *testing.T) {
	d := apitest.New(t)
	owner := client{t: t, base: d.URL, token: d.OwnerToken}
	head := client{t: t, base: d.URL, token: d.AgentToken(t, "head")}
	var l1, l2, l3 slots.Lease
	owner.do("POST", "/api/slots", map[string]any{"kind": "browser", "command": "playwright test"}, &l1)
	head.do("POST", "/api/slots", map[string]any{"kind": "browser", "command": "chromium"}, &l2)
	head.do("POST", "/api/slots", map[string]any{"kind": "browser", "command": "chromium 2"}, &l3)
	if l1.State != slots.Held || l2.State != slots.Held || l3.State != slots.Waiting || l3.Position != 1 || l2.Holder != "head" {
		t.Fatalf("leases %+v %+v %+v", l1, l2, l3)
	}
	if s := owner.do("POST", "/api/slots", map[string]any{"kind": "gpu"}, nil); s != 400 {
		t.Fatalf("unknown kind: %d", s)
	}
	var w slots.Lease
	if head.do("POST", "/api/slots/"+l3.ID+"/wait?timeout=1", nil, &w) != 200 || w.State != slots.Waiting {
		t.Fatalf("wait timeout: %+v", w)
	}
	if s := head.do("DELETE", "/api/slots/"+l1.ID, nil, nil); s != 403 {
		t.Fatalf("agent released the owner's lease: %d", s)
	}
	owner.do("DELETE", "/api/slots/"+l1.ID, nil, nil)
	if head.do("POST", "/api/slots/"+l3.ID+"/wait?timeout=5", nil, &w) != 200 || w.State != slots.Held {
		t.Fatalf("wait after release: %+v", w)
	}
	var v slots.View
	if head.do("GET", "/api/slots", nil, &v) != 200 || len(v.Leases) != 2 {
		t.Fatalf("view: %+v", v)
	}
	if s := head.do("POST", "/api/slots/nope/renew", nil, nil); s != 404 {
		t.Fatalf("renew unknown: %d", s)
	}
}

func TestHealthShowsMachineProtection(t *testing.T) {
	d := apitest.New(t)
	var h struct {
		MemGuard struct {
			Enabled bool   `json:"enabled"`
			Level   string `json:"level"`
		} `json:"memguard"`
		DiskReserve struct {
			Mode  string `json:"mode"`
			State string `json:"state"`
		} `json:"disk_reserve"`
	}
	if (client{t: t, base: d.URL}).do("GET", "/api/health", nil, &h) != 200 || !h.MemGuard.Enabled || h.MemGuard.Level == "" ||
		h.DiskReserve.Mode != "on" || h.DiskReserve.State == "" {
		t.Fatalf("health: %+v", h)
	}
	var v resources.View
	if (client{t: t, base: d.URL, token: d.OwnerToken}).do("GET", "/api/resources", nil, &v) != 200 || v.Protect.DiskReserve.Mode != "on" {
		t.Fatalf("protect in the resources view: %+v", v.Protect)
	}
}
