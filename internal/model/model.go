// Package model holds the types shared by the daemon, the API, the CLI and (mirrored in
// ui/src/lib/types.ts) the UI. JSON names are the contract; see DESIGN.md.
package model

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

type Level string

const (
	LevelHead   Level = "head"
	LevelLead   Level = "lead"
	LevelWorker Level = "worker"
)

type Harness string

const (
	HarnessClaude   Harness = "claude"
	HarnessShell    Harness = "shell"
	HarnessCodex    Harness = "codex"
	HarnessOpenCode Harness = "opencode"
	HarnessPi       Harness = "pi"
	HarnessGemini   Harness = "gemini"
)

type Mode string

const (
	ModeChat     Mode = "chat"
	ModeTerminal Mode = "terminal"
)

type AgentState string

const (
	StateStarting AgentState = "starting"
	StateWorking  AgentState = "working"
	StateIdle     AgentState = "idle"
	StateWaiting  AgentState = "waiting" // a permission request or question is pending
	StateStopped  AgentState = "stopped"
	StateExited   AgentState = "exited"
	StateError    AgentState = "error"
	// Resource control (DESIGN.md "Resources"):
	StateQueued AgentState = "queued" // waits for capacity (max agents, free RAM); starts in order
	StatePaused AgentState = "paused" // its process tree is suspended by a brake; PausedReason says why
)

// Owner is the From/To value for the human.
const Owner = "owner"

// CompanionParty is the From of mail from Companion, the owner's desktop agent app (a message
// it sent through `tmx mcp --companion`, or the outcome of a task tmx handed to it).
const CompanionParty = "companion"

type Agent struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	ProjectID string  `json:"project_id,omitempty"`
	ParentID  string  `json:"parent_id,omitempty"`
	Level     Level   `json:"level"`
	Profile   string  `json:"profile"` // profile slug; "" in old rows = the built-in of the level
	Harness   Harness `json:"harness"`
	// Account is the harness account the agent runs as (internal/accounts; "" = the
	// harness's usual home, as before there were several).
	Account        string     `json:"account,omitempty"`
	Mode           Mode       `json:"mode"`
	Model          string     `json:"model"`
	Cwd            string     `json:"cwd"`
	Role           string     `json:"role"`
	Brief          string     `json:"brief"`
	State          AgentState `json:"state"`
	Desired        string     `json:"desired"` // running | stopped
	SessionID      string     `json:"session_id"`
	PID            int        `json:"pid"`
	CreatedAt      int64      `json:"created_at"`
	UpdatedAt      int64      `json:"updated_at"`
	LastActivityAt int64      `json:"last_activity_at"`
	ShelvedAt      int64      `json:"shelved_at,omitempty"`
	DoneAt         int64      `json:"done_at,omitempty"`
	Protected      bool       `json:"protected,omitempty"`
	CostUSD        float64    `json:"cost_usd"`
	Turns          int        `json:"turns"`
	Error          string     `json:"error"`
	Queued         int        `json:"queued"`
	// Resource control; kept in memory (not in the store), rebuilt after a restart.
	QueuePos     int    `json:"queue_pos,omitempty"`     // state queued: 1 = next to start
	QueueReason  string `json:"queue_reason,omitempty"`  // state queued: what it waits for
	PausedReason string `json:"paused_reason,omitempty"` // state paused: write_budget | memory | disk
	PausedDetail string `json:"paused_detail,omitempty"` // state paused: a sentence for the owner
	PausedAt     int64  `json:"paused_at,omitempty"`
	// Live mode switch (DESIGN.md "Switching live"); kept in memory, not in the store, so a
	// daemon restart brings the agent back in the mode it was launched in.
	HomeMode    Mode `json:"home_mode,omitempty"`    // set while switched: the mode it was launched in
	PendingMode Mode `json:"pending_mode,omitempty"` // it switches to this mode when its current turn ends

	CodexFullAccess bool `json:"codex_full_access,omitempty"`
	AgyFullAccess   bool `json:"agy_full_access,omitempty"` // owner opt-in; runtime only, reset on daemon restart
}

type ProjectStatus string

const (
	ProjectActive   ProjectStatus = "active"
	ProjectPaused   ProjectStatus = "paused"
	ProjectArchived ProjectStatus = "archived"
)

type Project struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Slug      string        `json:"slug"`
	Folder    string        `json:"folder"`
	Brief     string        `json:"brief"`
	LeadID    string        `json:"lead_id"`
	Status    ProjectStatus `json:"status"`
	CreatedAt int64         `json:"created_at"`
	UpdatedAt int64         `json:"updated_at"`

	// Checklist progress (see "Progress tracking" below): the stored, optimistically-versioned
	// fields a writer patches with UpdateProgress. Progress is nil when nothing was ever
	// reported. ViewProgress derives the read-only view (tally + conservative ETA) from these.
	Progress          *int          `json:"progress,omitempty"`
	ProgressBasis     ProgressBasis `json:"progress_basis,omitempty"`
	ProgressState     ProgressState `json:"progress_state,omitempty"`
	ProgressVersion   int           `json:"progress_version"`
	ProgressUpdatedAt int64         `json:"progress_updated_at,omitempty"`
	ProgressRun       string        `json:"progress_run,omitempty"`
}

// Progress tracking (DESIGN.md "Progress"): a checklist-backed percent complete per project,
// stored with an optimistic version so two writers never silently clobber each other, plus
// enough recent samples (kept by the store) to derive a conservative ETA. Mirrors the Python
// reference contract (ratelimits/tmx/src/tmx/progress.py) field for field.

type ProgressState string

const (
	ProgressActive   ProgressState = "active"
	ProgressBlocked  ProgressState = "blocked"
	ProgressPaused   ProgressState = "paused"
	ProgressComplete ProgressState = "complete"
)

var progressIDRx = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ProgressStep is one checklist line.
type ProgressStep struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Done  bool   `json:"done"`
}

// ProgressBasis is the checklist a reported percent is computed from. Revision names its
// shape (the set of step ids/labels): changing which steps exist requires a new revision, so
// an agent can never silently redefine what "50%" means under an old one.
type ProgressBasis struct {
	Revision string         `json:"revision"`
	Steps    []ProgressStep `json:"steps"`
}

// ErrProgressConflict is returned by UpdateProgress (and by the store when persisting) when
// the caller's ExpectedVersion is stale: someone else updated progress first and the caller
// must re-read before retrying.
var ErrProgressConflict = errors.New("progress changed concurrently; reload before updating")

// ProgressPatch is a requested change to a project's progress. The Has* flags distinguish "not
// mentioned" from "explicitly set", exactly like the Python reference's dict-shaped patch: an
// update that mentions none of Clear/Percent/Basis/State/Confirm is a no-op regardless of
// ExpectedVersion.
type ProgressPatch struct {
	ExpectedVersion int
	Clear           bool // Percent: null in the Python contract — wipes the checklist entirely
	Percent         int
	HasPercent      bool
	Basis           ProgressBasis
	HasBasis        bool
	State           ProgressState
	HasState        bool
	Confirm         bool
	HasConfirm      bool
}

func (p ProgressPatch) touches() bool {
	return p.Clear || p.HasPercent || p.HasBasis || p.HasState || p.HasConfirm
}

// progressStepShape is a checklist's shape (ordered step ids + labels, ignoring done): two
// bases with the same revision must share this, or the revision no longer names one fixed
// checklist.
func progressStepShape(b ProgressBasis) string {
	var sb strings.Builder
	for _, s := range b.Steps {
		sb.WriteString(s.ID)
		sb.WriteByte(0)
		sb.WriteString(s.Label)
		sb.WriteByte(1)
	}
	return sb.String()
}

func validateProgressBasis(b ProgressBasis) error {
	if !progressIDRx.MatchString(b.Revision) {
		return errors.New("progress_basis.revision must be a short stable identifier")
	}
	if len(b.Steps) < 1 || len(b.Steps) > 40 {
		return errors.New("progress_basis.steps must contain 1 to 40 steps")
	}
	seen := make(map[string]bool, len(b.Steps))
	for _, s := range b.Steps {
		if !progressIDRx.MatchString(s.ID) || seen[s.ID] {
			return errors.New("progress step ids must be unique short identifiers")
		}
		seen[s.ID] = true
		if strings.TrimSpace(s.Label) == "" || len(s.Label) > 120 {
			return errors.New("progress labels must contain 1 to 120 characters")
		}
		// Cf (Unicode "format") runes — bidi overrides, zero-width joiners, and the like — can
		// make a label render as something other than its actual text; never accepted, since a
		// later UI trusts this string enough to show it as-is.
		for _, r := range s.Label {
			if unicode.Is(unicode.Cf, r) {
				return errors.New("progress labels must not contain unicode format control characters")
			}
		}
	}
	return nil
}

// UpdateProgress applies patch to cur's stored progress fields and returns the result (cur
// with just those fields replaced; every other field is untouched). It never mutates cur.
//
// Semantics match the Python reference's progress.update:
//   - a patch that mentions no progress field is a pass-through; ExpectedVersion isn't even
//     checked.
//   - otherwise ExpectedVersion must equal cur.ProgressVersion or this returns
//     ErrProgressConflict.
//   - Clear wipes the checklist (percent, basis, state, run) and bumps the version.
//   - otherwise percent/basis default to cur's when the patch doesn't set them, percent must
//     equal the rounded completed-step fraction, and reusing a revision with a different set
//     of steps is rejected (a revision's shape is immutable once used).
//   - state defaults to complete at 100%, else carries over from cur (downgrading a stale
//     "complete" left over from before the checklist reopened below 100%); complete requires
//     exactly 100%.
//   - a write that changes nothing and isn't Confirm=true is a no-op (no version bump).
//   - ProgressRun is a fresh id whenever history can't be read as one continuous run: first
//     write, a new revision, percent regressing, a state change, or the clock going backward.
func UpdateProgress(cur Project, patch ProgressPatch, now int64) (Project, error) {
	if !patch.touches() {
		return cur, nil
	}
	if patch.ExpectedVersion != cur.ProgressVersion {
		return cur, ErrProgressConflict
	}
	out := cur
	if patch.Clear {
		out.Progress = nil
		out.ProgressBasis = ProgressBasis{}
		out.ProgressState = ""
		out.ProgressRun = ""
		out.ProgressVersion = cur.ProgressVersion + 1
		out.ProgressUpdatedAt = now
		return out, nil
	}
	var percent int
	switch {
	case patch.HasPercent:
		percent = patch.Percent
	case cur.Progress != nil:
		percent = *cur.Progress
	default:
		return cur, errors.New("progress must be an integer from 0 to 100, or null to clear")
	}
	if percent < 0 || percent > 100 {
		return cur, errors.New("progress must be an integer from 0 to 100, or null to clear")
	}
	basis := cur.ProgressBasis
	if patch.HasBasis {
		basis = patch.Basis
	}
	if err := validateProgressBasis(basis); err != nil {
		return cur, err
	}
	done := 0
	for _, s := range basis.Steps {
		if s.Done {
			done++
		}
	}
	if want := int(math.Floor(100*float64(done)/float64(len(basis.Steps)) + 0.5)); percent != want {
		return cur, errors.New("progress must equal the rounded completed checklist fraction")
	}
	if len(cur.ProgressBasis.Steps) > 0 && cur.ProgressBasis.Revision == basis.Revision &&
		progressStepShape(cur.ProgressBasis) != progressStepShape(basis) {
		return cur, errors.New("changed checklist scope requires a new revision")
	}
	var state ProgressState
	switch {
	case patch.HasState:
		state = patch.State
	case percent == 100:
		state = ProgressComplete
	default:
		state = cur.ProgressState
		if state == "" || state == ProgressComplete {
			state = ProgressActive
		}
	}
	switch state {
	case ProgressActive, ProgressBlocked, ProgressPaused, ProgressComplete:
	default:
		return cur, errors.New("progress_state must be active, blocked or paused; complete requires exactly 100%")
	}
	if (state == ProgressComplete) != (percent == 100) {
		return cur, errors.New("progress_state must be active, blocked or paused; complete requires exactly 100%")
	}
	curPercent := 0
	if cur.Progress != nil {
		curPercent = *cur.Progress
	}
	changed := cur.Progress == nil || curPercent != percent ||
		!reflect.DeepEqual(cur.ProgressBasis, basis) || cur.ProgressState != state
	if !changed && !(patch.HasConfirm && patch.Confirm) {
		return cur, nil
	}
	reset := cur.ProgressRun == "" || cur.ProgressBasis.Revision != basis.Revision ||
		percent < curPercent || state != cur.ProgressState || now < cur.ProgressUpdatedAt
	out.Progress = &percent
	out.ProgressBasis = basis
	out.ProgressState = state
	out.ProgressVersion = cur.ProgressVersion + 1
	out.ProgressUpdatedAt = now
	if reset {
		out.ProgressRun = NewID() + NewID()
	} else {
		out.ProgressRun = cur.ProgressRun
	}
	return out, nil
}

// ProgressSample is one historical point the store keeps for a project's progress run, used to
// compute a conservative ETA. Version lets same-instant samples stay ordered.
type ProgressSample struct {
	Version int
	At      int64
	Run     string
	Percent int
}

// DefaultStaleAfterHours is how long a non-complete project can go without a progress update
// before it's considered stale (no ETA, surfaced to the owner).
const DefaultStaleAfterHours = 4

// ProgressView is the read-only, derived view of a project's progress: the checklist tally
// and, only when recent, active and well-behaved, a conservative ETA. Never stored.
type ProgressView struct {
	Percent    *int   `json:"percent"`
	Done       *int   `json:"done,omitempty"`
	Total      *int   `json:"total,omitempty"`
	State      string `json:"state"`
	UpdatedAt  int64  `json:"updated_at,omitempty"`
	Version    int    `json:"version"`
	Stale      bool   `json:"stale"`
	ETASeconds *int64 `json:"eta_seconds,omitempty"`
	ETAAt      *int64 `json:"eta_at,omitempty"`
	ETAReason  string `json:"eta_reason"`
}

// ViewProgress derives a project's progress view from its stored fields and recent samples
// (only those from the same progress run count). now and every sample/field timestamp are
// Unix milliseconds (model.Now()); ETASeconds/ETAReason are the exception, kept in whole
// seconds since they're for display. staleAfterHours outside (0, 720] falls back to
// DefaultStaleAfterHours, matching the Python reference's bounds-check.
//
// The ETA is deliberately conservative: it needs at least 3 advancing samples spanning at
// least 10 minutes within the last 24h, a steady rate (the fastest no more than 4x the
// slowest), and the project must be active, fresh and not stuck ahead of the clock —
// otherwise it reports why not (ETAReason) instead of guessing.
func ViewProgress(p Project, samples []ProgressSample, now int64, staleAfterHours float64) ProgressView {
	if !(staleAfterHours > 0 && staleAfterHours <= 720) {
		staleAfterHours = DefaultStaleAfterHours
	}
	out := ProgressView{State: "unreported", Version: p.ProgressVersion, ETAReason: "not-reported"}
	if p.Progress == nil {
		return out
	}
	done, total := 0, len(p.ProgressBasis.Steps)
	for _, s := range p.ProgressBasis.Steps {
		if s.Done {
			done++
		}
	}
	out.Percent = p.Progress
	out.Done, out.Total = &done, &total
	out.State = string(p.ProgressState)
	out.UpdatedAt = p.ProgressUpdatedAt
	age := now - p.ProgressUpdatedAt
	stale := p.ProgressState != ProgressComplete && age >= int64(staleAfterHours*float64(time.Hour/time.Millisecond))
	out.Stale = stale
	if p.ProgressState != ProgressActive || stale || age < 0 {
		switch {
		case stale:
			out.ETAReason = "stale"
		case age < 0:
			out.ETAReason = "clock-changed"
		default:
			out.ETAReason = string(p.ProgressState)
		}
		return out
	}
	const dayMs = int64(24 * time.Hour / time.Millisecond)
	var points []ProgressSample
	for _, s := range samples {
		if s.Run == p.ProgressRun && now-dayMs <= s.At && s.At <= now {
			points = append(points, s)
		}
	}
	sort.Slice(points, func(i, j int) bool {
		if points[i].At != points[j].At {
			return points[i].At < points[j].At
		}
		return points[i].Version < points[j].Version
	})
	var advancing []ProgressSample
	for _, s := range points {
		if len(advancing) == 0 || s.Percent > advancing[len(advancing)-1].Percent {
			advancing = append(advancing, s)
		}
	}
	if len(advancing) > 6 {
		advancing = advancing[len(advancing)-6:]
	}
	const minSpanMs = int64(10 * time.Minute / time.Millisecond)
	if len(advancing) < 3 || advancing[len(advancing)-1].At-advancing[0].At < minSpanMs {
		out.ETAReason = "insufficient-history"
		return out
	}
	// rates are in percent per second (not per ms): ETASeconds below is a human-facing
	// duration, and keeping the rate in whole seconds avoids float noise from ms deltas.
	var rates []float64
	for i := 0; i < len(advancing)-1; i++ {
		a, b := advancing[i], advancing[i+1]
		if b.At > a.At {
			rates = append(rates, float64(b.Percent-a.Percent)/(float64(b.At-a.At)/1000))
		}
	}
	if len(rates) == 0 {
		out.ETAReason = "no-rate"
		return out
	}
	minR, maxR := rates[0], rates[0]
	for _, r := range rates[1:] {
		if r < minR {
			minR = r
		}
		if r > maxR {
			maxR = r
		}
	}
	switch {
	case len(rates) < 2 || minR <= 0:
		out.ETAReason = "no-rate"
	case maxR > 4*minR:
		out.ETAReason = "uneven-rate"
	default:
		const roundSeconds = 300
		seconds := int64(math.Ceil(float64(100-*p.Progress)/medianFloat(rates)/roundSeconds) * roundSeconds)
		etaAt := now + seconds*1000
		out.ETASeconds, out.ETAAt = &seconds, &etaAt
		out.ETAReason = "recent-pace"
	}
	return out
}

func medianFloat(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

type MessageState string

const (
	MsgQueued    MessageState = "queued"
	MsgDelivered MessageState = "delivered"
	MsgRead      MessageState = "read"
	MsgFailed    MessageState = "failed"
)

type Message struct {
	RetryID     string       `json:"retry_id,omitempty"`
	RetryHash   string       `json:"-"`
	Receipt     string       `json:"receipt,omitempty"` // queued, dispatching, accepted, submitted, unconfirmed, read, owner
	ReceiptAt   int64        `json:"receipt_at,omitempty"`
	ID          string       `json:"id"`
	From        string       `json:"from"` // agent ID or "owner"
	To          string       `json:"to"`   // agent ID or "owner"
	Body        string       `json:"body"`
	State       MessageState `json:"state"`
	ReplyTo     string       `json:"reply_to,omitempty"`
	CreatedAt   int64        `json:"created_at"`
	DeliveredAt int64        `json:"delivered_at,omitempty"`
	// ThreadID: the board topic of mail to or from the head ("" = none given).
	ThreadID string `json:"thread_id,omitempty"`
}

type ApprovalKind string

const (
	ApprovalPermission ApprovalKind = "permission"
	ApprovalQuestion   ApprovalKind = "question"
	ApprovalGuard      ApprovalKind = "guard" // a tool call the dangerous-command guard held; Input is a GuardHold
	// ApprovalBrake: an agent was paused by a resource brake; decisions resume | raise | deny.
	ApprovalBrake ApprovalKind = "brake"
	// ApprovalCompanion: about a task for Companion (DESIGN.md "Companion"). Tool
	// ToolCompanionTask: an agent wants to hand a task over (allow | deny; Input is a
	// CompanionApprovalInput with the exact text). Tool ToolCompanionMessage: the head's
	// message to Companion reads like it asks Companion to act (allow | deny; the exact text,
	// as for a task). Tool ToolCompanionQuestion: Companion asks about a task (answer | deny =
	// cancel the task).
	ApprovalCompanion ApprovalKind = "companion"
)

// Tools of companion approvals.
const (
	ToolCompanionTask     = "companion_task"
	ToolCompanionMessage  = "companion_message"
	ToolCompanionQuestion = "companion_question"
)

// GuardHold is the Input of an Approval of kind guard, and meta["guard"] of the timeline
// entries the guard writes (held, decided, blocked).
type GuardHold struct {
	Outcome   string          `json:"outcome"` // hold | block
	Rule      string          `json:"rule"`
	RuleName  string          `json:"rule_name"`
	Why       string          `json:"why"`
	Detail    string          `json:"detail"`
	Match     string          `json:"match"`   // the part of the command that matched
	Command   string          `json:"command"` // the whole command, or the file path for file tools
	Tool      string          `json:"tool"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Cwd       string          `json:"cwd,omitempty"`
}

type ApprovalState string

const (
	ApprovalPending  ApprovalState = "pending"
	ApprovalAllowed  ApprovalState = "allowed"
	ApprovalDenied   ApprovalState = "denied"
	ApprovalAnswered ApprovalState = "answered"
	ApprovalExpired  ApprovalState = "expired"
)

// ToolTerminalMenu is Approval.Tool for a menu read off a terminal agent's screen (a
// permission prompt, question or dialog of a harness TUI). Question holds the dialog text,
// Options its choices; the answer is typed as keys.
const ToolTerminalMenu = "terminal_menu"

type Approval struct {
	Deferred    bool            `json:"deferred,omitempty"`
	AnswerID    string          `json:"answer_id,omitempty"`
	Version     int64           `json:"version"`
	Card        *OwnerCard      `json:"card,omitempty"`
	ID          string          `json:"id"`
	AgentID     string          `json:"agent_id"`
	Kind        ApprovalKind    `json:"kind"`
	Tool        string          `json:"tool"`
	Input       json.RawMessage `json:"input,omitempty"`
	Description string          `json:"description"`
	Question    string          `json:"question"`
	Options     []string        `json:"options"`
	State       ApprovalState   `json:"state"`
	Answer      string          `json:"answer"`
	CreatedAt   int64           `json:"created_at"`
	DecidedAt   int64           `json:"decided_at,omitempty"`
	// ThreadID: the board topic a head's question was asked in (its answer goes there).
	ThreadID string `json:"thread_id,omitempty"`
}

type EntryKind string

const (
	EntryUser       EntryKind = "user"
	EntryAssistant  EntryKind = "assistant"
	EntryThinking   EntryKind = "thinking"
	EntryToolUse    EntryKind = "tool_use"
	EntryToolResult EntryKind = "tool_result"
	EntrySystem     EntryKind = "system"
	EntryResult     EntryKind = "result"
	EntryMessageIn  EntryKind = "message_in"
	EntryError      EntryKind = "error"
)

type TimelineEntry struct {
	Seq       int64           `json:"seq"`
	AgentID   string          `json:"agent_id"`
	At        int64           `json:"at"`
	Kind      EntryKind       `json:"kind"`
	Text      string          `json:"text"`
	Tool      string          `json:"tool,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Meta      map[string]any  `json:"meta,omitempty"`
	// ThreadID: the board topic (DESIGN.md "Board") of a head's entry; "" = general.
	ThreadID string `json:"thread_id,omitempty"`
}

type LevelPolicy struct {
	Model             string   `json:"model"`
	AllowedModels     []string `json:"allowed_models"`
	PermissionMode    string   `json:"permission_mode"`
	CanCreateProjects bool     `json:"can_create_projects"`
	CanLaunch         []Level  `json:"can_launch"`
}

type Policy struct {
	Levels map[Level]LevelPolicy `json:"levels"`
}

// DefaultPolicy: head on Opus, everyone below on Sonnet (owner's rule, 21 Oct 2026).
func DefaultPolicy() Policy {
	return Policy{Levels: map[Level]LevelPolicy{
		LevelHead: {Model: "claude-opus-5-5", AllowedModels: []string{"claude-opus-5-5"},
			PermissionMode: "auto", CanCreateProjects: true, CanLaunch: []Level{LevelLead, LevelWorker}},
		LevelLead: {Model: "claude-opus-5-5", AllowedModels: []string{"claude-opus-5-5", "claude-sonnet-5"},
			PermissionMode: "auto", CanLaunch: []Level{LevelWorker}},
		LevelWorker: {Model: "claude-sonnet-5", AllowedModels: []string{"claude-sonnet-5", "claude-opus-5-5", "claude-haiku-4-5-20251001"},
			PermissionMode: "auto"},
	}}
}

type Snapshot struct {
	RollbackWarning string     `json:"rollback_warning,omitempty"`
	Machine         string     `json:"machine"`
	Version         string     `json:"version"`
	Head            *Agent     `json:"head"`
	Projects        []Project  `json:"projects"`
	Agents          []Agent    `json:"agents"`
	Approvals       []Approval `json:"approvals"`
	Policy          Policy     `json:"policy"` // derived from the built-in profiles
	Profiles        []Profile  `json:"profiles"`
	Workspace       string     `json:"workspace"`
}

// Bus event types (WebSocket /api/events).
const (
	EvSnapshot         = "snapshot"
	EvAgentUpdated     = "agent.updated"
	EvAgentRemoved     = "agent.removed"
	EvProjectUpdated   = "project.updated"
	EvProjectRemoved   = "project.removed"
	EvTimelineAppended = "timeline.appended"
	EvApprovalCreated  = "approval.created"
	EvApprovalResolved = "approval.resolved"
	EvMessageCreated   = "message.created"
	EvMessageUpdated   = "message.updated"
	EvResourcesSampled = "resources.sampled" // data: resources.Sample (every few seconds)
	EvResourcesGuard   = "resources.guard"   // data: resources.GuardEvent (memory guard / disk reserve)
	EvSlotsUpdated     = "slots.updated"     // data: slots.View
	EvCompanionTask    = "companion.task"    // data: CompanionTask (new or changed)
	EvThreadUpdated    = "thread.updated"    // data: Thread (new or changed)
	EvBoardChanged     = "board.changed"     // head turns moved between threads; data: {"thread_id"}
	EvThreadRemoved    = "thread.removed"    // a topic was deleted; data: {"id"}
	EvThreadEntry      = "thread.entry"      // a line was added to a thread's log; data: ThreadEntry
	EvThreadLink       = "thread.link"       // a link was attached or ended (the Matrix); data: ThreadLink
)

type BusEvent struct {
	Type      string `json:"type"`
	AgentID   string `json:"agent_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Data      any    `json:"data"`
	At        int64  `json:"at"`
}

var nameRx = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ValidName reports whether s is a valid agent name.
func ValidName(s string) bool { return nameRx.MatchString(s) }

// Now returns the current time in Unix milliseconds.
func Now() int64 { return time.Now().UnixMilli() }

const idAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

// NewID returns a 10-character random lowercase ID.
func NewID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}
