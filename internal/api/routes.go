package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/blackcode-ch/tmx-go/internal/fleet"
	"github.com/blackcode-ch/tmx-go/internal/model"
)

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/remote", s.remote)
	s.agentControlRoutes(mux)
	s.agentProjectRoutes(mux)
	s.agentFileRoutes(mux)
	s.messagePinRoutes(mux)
	s.companionMailboxRoutes(mux)
	s.taskRoutes(mux)
	s.processRoutes(mux)
	s.jobRoutes(mux)
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/peers", s.ownerOnly(s.peerStatus))
	mux.HandleFunc("POST /api/peers/{name}/rotate", s.ownerOnly(s.peerRotate))
	mux.HandleFunc("GET /api/snapshot", s.snapshot)
	mux.HandleFunc("GET /api/fleet", s.fleetRoster)
	mux.HandleFunc("GET /api/sessions", s.peerSessions)
	mux.HandleFunc("POST /api/shutdown", s.ownerOnly(s.shutdown))

	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("GET /api/projects/{id}/section", s.getProjectSection)
	mux.HandleFunc("GET /api/projects/{id}/artifacts", s.getProjectArtifacts)
	mux.HandleFunc("POST /api/projects/{id}/contribution", s.setProjectContribution)
	mux.HandleFunc("POST /api/projects/{id}/section", s.setProjectSection)
	mux.HandleFunc("POST /api/projects/{id}/import", s.importProject)
	mux.HandleFunc("POST /api/projects/{id}/members", s.addProjectMember)
	mux.HandleFunc("POST /api/projects/{id}/feedback", s.ownerOnly(s.projectFeedback))
	mux.HandleFunc("PATCH /api/projects/{id}", s.ownerOnly(s.patchProject))
	mux.HandleFunc("DELETE /api/projects/{id}", s.ownerOnly(s.deleteProject))

	mux.HandleFunc("POST /api/agents", s.launch)
	mux.HandleFunc("GET /api/agents/{id}", s.getAgent)
	mux.HandleFunc("DELETE /api/agents/{id}", s.ownerOnly(s.removeAgent))
	mux.HandleFunc("POST /api/agents/{id}/start", s.start) // resources.go: force / no_wait
	mux.HandleFunc("POST /api/agents/{id}/stop", s.act((*fleet.Fleet).StopAgent))
	mux.HandleFunc("POST /api/agents/{id}/done", s.act((*fleet.Fleet).MarkDone))
	mux.HandleFunc("POST /api/agents/{id}/restart", s.restart) // lifecycle.go: fresh / prompt
	mux.HandleFunc("POST /api/agents/{id}/interrupt", s.act((*fleet.Fleet).InterruptAgent))
	mux.HandleFunc("POST /api/agents/{id}/send", s.ownerOnly(s.send))
	mux.HandleFunc("POST /api/agents/{id}/uploads", s.ownerOnly(s.upload)) // uploads.go
	mux.HandleFunc("GET /api/uploads/{id}", s.ownerOnly(s.getUpload))
	mux.HandleFunc("GET /api/agents/{id}/timeline", s.timeline)
	mux.HandleFunc("GET /api/agents/{id}/terminal", s.terminal)
	mux.HandleFunc("GET /api/agents/{id}/screen", s.screen)  // terminal.go: Fleet.Screen
	mux.HandleFunc("POST /api/agents/{id}/type", s.typeText) // terminal.go: Fleet.TypeText
	mux.HandleFunc("POST /api/agents/{id}/keys", s.sendKeys) // terminal.go: Fleet.SendKeys

	mux.HandleFunc("GET /api/approvals", s.ownerOnly(s.listApprovals))
	mux.HandleFunc("POST /api/approvals", s.ask)
	mux.HandleFunc("POST /api/approvals/{id}", s.ownerOnly(s.decide))
	mux.HandleFunc("POST /api/approvals/{id}/seen", s.ownerOnly(s.seenApproval))

	mux.HandleFunc("GET /api/messages", s.listMessages)
	mux.HandleFunc("POST /api/messages", s.postMessage)

	mux.HandleFunc("GET /api/policy", s.ownerOnly(s.getPolicy))
	mux.HandleFunc("PUT /api/policy", s.ownerOnly(s.putPolicy))

	mux.HandleFunc("GET /api/config", s.ownerOnly(s.getConfig))
	mux.HandleFunc("PUT /api/config", s.ownerOnly(s.putConfig))
	s.profileRoutes(mux)

	mux.HandleFunc("GET /api/events", s.events)
	s.resourceRoutes(mux)
	s.safetyRoutes(mux)
	s.gatePINRoutes(mux) // gatepin.go (#187)
	s.chatRoutes(mux)
	s.chatBoardRoutes(mux) // chatboards.go
	s.p2pRoutes(mux)
	s.updateRoutes(mux) // update.go
	s.boardRoutes(mux)  // board.go
	s.matrixRoutes(mux) // matrix.go
	s.companionRoutes(mux)
	s.modeRoutes(mux)      // mode.go: POST /api/agents/{id}/mode
	s.voiceRoutes(mux)     // transcribe.go
	s.recordingRoutes(mux) // recordings.go
	s.summaryRoutes(mux)   // summaries.go
	s.whatsappRoutes(mux)  // whatsapp.go
	s.accountRoutes(mux)   // accounts.go
	s.fleetHostRoutes(mux) // fleethosts.go

	mux.HandleFunc("GET /api/harnesses", s.harnesses)
	mux.HandleFunc("GET /api/discover", s.ownerOnly(s.discover))
	mux.HandleFunc("POST /api/adopt", s.ownerOnly(s.adopt))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such API route: "+r.Method+" "+r.URL.Path)
	})
	s.skillRoutes(mux) // skill.go: GET /skill.md
	mux.Handle("/", uiHandler())
}

// ownerOnly wraps a handler that only the owner may call.
func (s *Server) ownerOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, _ := caller(r); !c.Owner {
			writeError(w, http.StatusForbidden, "only the owner can do this")
			return
		}
		h(w, r)
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	// host and commit as the Python daemons answer them: a peer checks host against its name for
	// the machine, and doctor compares commit.
	v := map[string]any{"ok": true, "version": s.f.Version(), "machine": s.f.Machine(),
		"host": strings.ToLower(s.f.Machine()), "commit": buildCommit()}
	if s.peers != nil {
		v["host"] = s.peers.Machine() // the name the fleet's other machines know this one by
	}
	if s.jobs != nil {
		v["jobs_disabled"] = s.jobs.Disabled()
		v["governor_degraded"] = s.jobs.Degraded()
		v["job_persistence_healthy"] = s.jobs.PersistenceHealthy()
	}
	if s.res != nil {
		// Levels only (no agent or volume names): health is public. The same keys Python's
		// /api/health answers: memguard {enabled, level}, plus the disk reserve's mode and state.
		h := s.res.Health()
		v["memguard"], v["disk_reserve"] = h.MemGuard, h.DiskReserve
	}
	if s.updater != nil {
		// A bool only, no reason: health is public and peers read it. False = updates are off
		// because the pinned release keys or the require policy are invalid.
		v["updates_ok"] = s.updater.KeysOK()
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.f.Snapshot()
	if err != nil {
		writeErr(w, err)
		return
	}
	c, _ := caller(r)
	writeJSON(w, http.StatusOK, agentSnapshot(c, snap)) // whatsapp.go
}

func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	if s.stop == nil {
		writeError(w, http.StatusNotImplemented, "this server cannot be shut down over the API")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	go s.stop()
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req fleet.ProjectRequest
	if !decode(w, r, &req) {
		return
	}
	c, _ := caller(r)
	p, err := s.f.CreateProject(c, req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// addProjectMember is POST /api/projects/{id}/members {agent}: the owner, the head or the
// project's lead puts an existing agent into the project (fleet.AddProjectMember decides).
func (s *Server) addProjectMember(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Agent string `json:"agent"`
	}
	if !decode(w, r, &req) {
		return
	}
	c, _ := caller(r)
	a, err := s.f.AddProjectMember(c, r.PathValue("id"), req.Agent)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) patchProject(w http.ResponseWriter, r *http.Request) {
	var req fleet.ProjectPatch
	if !decode(w, r, &req) {
		return
	}
	c, _ := caller(r)
	p, err := s.f.UpdateProject(c, r.PathValue("id"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	if !s.gateDeleteProject(w, r, r.PathValue("id")) {
		return
	}
	c, _ := caller(r)
	if err := s.f.DeleteProject(c, r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) launch(w http.ResponseWriter, r *http.Request) {
	var req fleet.LaunchRequest
	if !decode(w, r, &req) {
		return
	}
	if req.AgyFullAccess && !s.RequireOwner(w, r, "let this agy agent skip its permission prompts", req) {
		return
	}
	c, _ := caller(r)
	a, err := s.f.LaunchAgent(c, req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := s.f.Agent(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) removeAgent(w http.ResponseWriter, r *http.Request) {
	c, _ := caller(r)
	if err := s.f.RemoveAgent(c, r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) act(fn func(*fleet.Fleet, fleet.Caller, string) (model.Agent, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, _ := caller(r)
		a, err := fn(s.f, c, r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	var o fleet.RestartOptions
	if r.ContentLength > 0 && !decode(w, r, &o) {
		return
	}
	c, _ := caller(r)
	a, err := s.f.RestartAgentWith(c, r.PathValue("id"), o)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) screen(w http.ResponseWriter, r *http.Request) {
	history, _ := strconv.Atoi(r.URL.Query().Get("history"))
	c, _ := caller(r)
	text, err := s.f.Screen(c, r.PathValue("id"), history)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

func (s *Server) typeText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text   string `json:"text"`
		Submit *bool  `json:"submit"`
	}
	if !decode(w, r, &req) {
		return
	}
	submit := req.Submit == nil || *req.Submit
	c, _ := caller(r)
	if err := s.f.TypeText(c, r.PathValue("id"), req.Text, submit); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) sendKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys string `json:"keys"`
	}
	if !decode(w, r, &req) {
		return
	}
	c, _ := caller(r)
	if err := s.f.SendKeys(c, r.PathValue("id"), req.Keys); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RetryID string   `json:"retry_id"`
		Text    string   `json:"text"`
		Uploads []string `json:"uploads"`   // ids from POST /api/agents/{id}/uploads
		Thread  string   `json:"thread_id"` // to the head: the board topic (board.go)
	}
	if !decode(w, r, &req) {
		return
	}
	text := req.Text
	if len(req.Uploads) > 0 {
		a, err := s.f.Agent(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		if text, err = s.attach(a.ID, text, req.Uploads); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	m, err := s.f.SendMessage(fleet.OwnerCaller, fleet.SendRequest{To: r.PathValue("id"), Body: text, ThreadID: req.Thread, RetryID: req.RetryID})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	entries, err := s.f.Timeline(r.PathValue("id"), after, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	c, _ := caller(r)
	writeJSON(w, http.StatusOK, map[string]any{"entries": agentTimeline(c, r.PathValue("id"), entries)}) // whatsapp.go
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	state := model.ApprovalState(r.URL.Query().Get("state"))
	if !r.URL.Query().Has("state") {
		state = model.ApprovalPending
	} else if state == "all" {
		state = ""
	}
	list, err := s.f.Approvals(state)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) ask(w http.ResponseWriter, r *http.Request) {
	var req fleet.AskRequest
	if !decode(w, r, &req) {
		return
	}
	c, _ := caller(r)
	ap, err := s.f.AskOwner(c, req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ap)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var req fleet.DecideRequest
	if !decode(w, r, &req) {
		return
	}
	if !s.gateDecide(w, r, r.PathValue("id"), req) {
		return
	}
	if ap, err := s.f.ApprovalByID(r.PathValue("id")); err == nil && !s.gateCompanion(w, r, ap, req) {
		return
	}
	c, _ := caller(r)
	ap, err := s.f.Decide(c, r.PathValue("id"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ap)
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	c, _ := caller(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	msgs, err := s.f.Messages(c, r.URL.Query().Get("agent"), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	if c, _ := caller(r); c.Companion {
		s.companionPostMail(w, r)
		return
	}
	var req fleet.SendRequest
	if !decode(w, r, &req) {
		return
	}
	c, _ := caller(r)
	m, err := s.f.SendMessage(c, req)
	if err != nil {
		writeCompanionErr(w, err) // the head's mail to Companion: Companion's refusals keep their status
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.f.Policy())
}

func (s *Server) putPolicy(w http.ResponseWriter, r *http.Request) {
	var p model.Policy
	if !decode(w, r, &p) {
		return
	}
	if !s.gatePolicy(w, r, p) {
		return
	}
	c, _ := caller(r)
	out, err := s.f.SetPolicy(c, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getProjectSection(w http.ResponseWriter, r *http.Request) {
	c, _ := caller(r)
	sec, err := s.f.ProjectSection(c, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sec)
}

func (s *Server) getProjectArtifacts(w http.ResponseWriter, r *http.Request) {
	c, _ := caller(r)
	out, err := s.f.ProjectArtifacts(c, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) setProjectContribution(w http.ResponseWriter, r *http.Request) {
	var req fleet.ProjectContributionPatch
	if !decode(w, r, &req) {
		return
	}
	c, _ := caller(r)
	out, err := s.f.SetMyProjectContribution(c, r.PathValue("id"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) setProjectSection(w http.ResponseWriter, r *http.Request) {
	var req fleet.ProjectSectionPatch
	if !decode(w, r, &req) {
		return
	}
	c, _ := caller(r)
	sec, err := s.f.SetProjectSection(c, r.PathValue("id"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sec)
}
func (s *Server) projectFeedback(w http.ResponseWriter, r *http.Request) {
	var req fleet.ProjectFeedbackRequest
	if !decode(w, r, &req) {
		return
	}
	c, _ := caller(r)
	rec, err := s.f.SendProjectFeedback(c, r.PathValue("id"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) seenApproval(w http.ResponseWriter, r *http.Request) {
	c, _ := caller(r)
	if err := s.f.MarkApprovalSeen(c, r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
