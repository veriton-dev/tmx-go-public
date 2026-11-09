package harness

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/blackcode-ch/tmx-go/internal/model"
	"github.com/blackcode-ch/tmx-go/internal/runner"
	"github.com/blackcode-ch/tmx-go/internal/runner/claude"
)

// Command is what terminal mode runs for a harness.
type Command struct {
	Path string
	Args []string
	Env  []string // added on top of the agent's env
	// LaunchID marks a new Codex conversation's developer instructions, so its rollout can
	// be found (CodexFindByMarker); "" otherwise.
	LaunchID string
}

// TerminalCommand builds the interactive TUI command for spec (not for a shell).
func TerminalCommand(spec runner.Spec) (Command, error) {
	path, err := LookPath(spec.Harness, spec.Command)
	if err != nil {
		return Command{}, err
	}
	c := Command{Path: path}
	switch spec.Harness {
	case model.HarnessClaude, "":
		c.Args = claude.TerminalArgs(spec)
		// No auto-update mid-session; no prompt suggestions: their grey ghost text in an empty
		// box read as a human's draft and held mail for hours (v2, 9 Oct).
		c.Env = []string{"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false"}
	case model.HarnessCodex:
		if spec.SessionID != "" {
			c.Args = append(c.Args, "resume", spec.SessionID)
		} else {
			c.LaunchID = newID()
		}
		// Its own process, not a client of the shared background app-server: stopping the
		// agent must stop its conversation.
		c.Args = append(c.Args, "--no-daemon")
		c.Args = append(c.Args, CodexArgs(spec, c.LaunchID)...)
		c.Args = append(c.Args, spec.ExtraArgs...)
	case model.HarnessOpenCode:
		// A private server per agent: OPENCODE_CONFIG_CONTENT (model, permission, MCP) is
		// only read by a server that this process starts; a shared background server would
		// ignore it. It also means stopping the agent stops its conversation.
		c.Args = append(c.Args, "--standalone")
		if spec.SessionID != "" {
			// opencode accepts an id up front and creates it if it does not exist yet.
			c.Args = append(c.Args, "-s", spec.SessionID)
		}
		c.Args = append(c.Args, spec.ExtraArgs...)
		c.Env = append(c.Env, "OPENCODE_DISABLE_AUTOUPDATE=1")
		cfg := map[string]any{}
		switch {
		case bypass(spec):
			cfg["permission"] = "allow"
		case spec.PermissionMode == "plan": // read-only: opencode's edit and bash permissions
			cfg["permission"] = map[string]any{"edit": "deny", "bash": "deny"}
		}
		if spec.Model != "" {
			// The TUI has no --model flag (only `opencode run` has one); its config does.
			cfg["model"] = spec.Model
		}
		if spec.MCP != nil {
			env := envValues(spec.Env, spec.MCP.EnvKeys)
			cfg["mcp"] = map[string]any{"tmxg": map[string]any{
				"type": "local", "command": append([]string{spec.MCP.Command}, spec.MCP.Args...),
				"environment": env, "enabled": true,
			}}
		}
		if len(cfg) > 0 {
			b, _ := json.Marshal(cfg)
			c.Env = append(c.Env, "OPENCODE_CONFIG_CONTENT="+string(b))
		}
	case model.HarnessPi:
		// --approve: project trust; pi has no tool-permission prompts at all.
		c.Args = append(c.Args, "--approve")
		if spec.SessionID != "" {
			c.Args = append(c.Args, "--session-id", spec.SessionID)
		}
		if spec.Model != "" {
			c.Args = append(c.Args, "--model", spec.Model)
		}
		c.Args = append(c.Args, spec.ExtraArgs...)
		c.Env = append(c.Env, "PI_SKIP_VERSION_CHECK=1", "PI_TELEMETRY=0")
	case model.HarnessGemini:
		// Gemini CLI's TUI: plan mode is its read-only mode; otherwise it asks before edits
		// and commands (default), or edits files by itself (auto_edit) under a bypass profile.
		switch {
		case spec.PermissionMode == "plan":
			c.Args = append(c.Args, "--approval-mode", "plan")
		case bypass(spec):
			c.Args = append(c.Args, "--approval-mode", "auto_edit")
		}
		if spec.Model != "" {
			c.Args = append(c.Args, "--model", spec.Model)
		}
		c.Args = append(c.Args, spec.ExtraArgs...)
		if spec.InitialPrompt != "" {
			// --prompt-interactive: runs the first message, then stays in the TUI
			c.Args = append(c.Args, "-i", spec.InitialPrompt)
		}
	case "agy", "antigravity":
		// Antigravity's terminal TUI. MCP/permissions are managed by agy itself; tmx
		// passes the model when one is chosen and any extra args.
		if spec.PermissionMode == "plan" {
			return Command{}, fmt.Errorf("agy cannot enforce a read-only profile")
		}
		if spec.AgyFullAccess {
			c.Args = append(c.Args, "--dangerously-skip-permissions")
		} else {
			c.Args = append(c.Args, "--mode", "auto")
		}
		if spec.Model != "" {
			c.Args = append(c.Args, "--model", spec.Model)
		}
		c.Args = append(c.Args, spec.ExtraArgs...)
		if spec.InitialPrompt != "" {
			c.Args = append(c.Args, "-i", spec.InitialPrompt)
		}
	default:
		return Command{}, fmt.Errorf("terminal mode does not know harness %q", spec.Harness)
	}
	return c, nil
}

// bypass: the policy lets the agent act without asking.
func bypass(spec runner.Spec) bool { return spec.PermissionMode == "bypassPermissions" }

// CodexArgs are the -c / permission / model options shared by the Codex TUI and app-server.
// launchID, when set, is added to the developer instructions as CodexLaunchMarker.
func CodexArgs(spec runner.Spec, launchID string) []string {
	args := []string{"-c", "check_for_update_on_startup=false"}
	sandbox, approval := CodexPolicy(spec)
	if sandbox == "" {
		// The owner opted this agent into full access: it already has it, so the confirmation
		// Codex shows when full access is picked again mid-session confirms nothing new (v2 and
		// tmx before #20 did the same). Agents without full access keep the confirmation, which
		// waits for the owner as a menu (no gate answers it).
		args = append(args, "--dangerously-bypass-approvals-and-sandbox", "-c", "notice.hide_full_access_warning=true")
	} else {
		args = append(args, "--sandbox", sandbox, "--ask-for-approval", approval)
	}
	args = append(args, CodexSandboxArgs(spec)...)
	if spec.Model != "" {
		args = append(args, "-m", spec.Model)
	}
	instr := spec.SystemPrompt
	if launchID != "" {
		instr = strings.TrimSpace(instr + "\n" + CodexLaunchMarker(launchID))
	}
	if instr != "" { // persisted as a developer message in the rollout
		args = append(args, "-c", "developer_instructions="+tomlString(instr))
	}
	args = append(args, codexMCPArgs(spec.MCP, spec.CodexGuardHome == "")...)
	return args
}

// CodexMCPArgs configures the tmx MCP server. Its secrets (the agent token) never go on the
// command line, which every process listing shows: Codex forwards them from its own
// environment by name (env_vars).
func CodexMCPArgs(m *runner.MCPServer) []string { return codexMCPArgs(m, true) }

func codexMCPArgs(m *runner.MCPServer, disableLegacy bool) []string {
	if m == nil {
		return nil
	}
	quoted := make([]string, len(m.Args))
	for i, a := range m.Args {
		quoted[i] = tomlString(a)
	}
	args := []string{
		"-c", "mcp_servers.tmxg.command=" + tomlString(m.Command),
		"-c", "mcp_servers.tmxg.args=[" + strings.Join(quoted, ", ") + "]",
	}
	if disableLegacy {
		args = append([]string{"-c", "mcp_servers.tmx.enabled=false"}, args...)
	}
	if len(m.EnvKeys) > 0 {
		keys := make([]string, len(m.EnvKeys))
		for i, k := range m.EnvKeys {
			keys[i] = tomlString(k)
		}
		args = append(args, "-c", "mcp_servers.tmxg.env_vars=["+strings.Join(keys, ", ")+"]")
	}
	return args
}

// codexGOOS is runtime.GOOS; tests set it to check another platform's policy.
var codexGOOS = runtime.GOOS

// CodexPolicy maps tmx's permission mode (Claude's names) to Codex's sandbox and approval
// policy; sandbox "" = the owner's explicit per-agent opt-in to bypass both.
//
// Windows never gets Codex's own sandbox: it rewrites file permissions (ACLs) on the folders
// it guards, and on 9 Sep it damaged a drive's file system metadata (v2's incident). There
// Codex runs unsandboxed (danger-full-access: it can write anywhere its user can) and asks,
// as before #20: on-request, and for a read-only profile untrusted (anything but a known
// read-only command waits for the owner). The launch checks (ValidateCodexLaunch) still
// refuse a root, home or system working folder, but do not confine later writes. Nothing
// sets spec.CodexWindowsSandbox today (no config key, no API field): it is the seam for a
// future opt-in, to be wired only after the VM check in CodexSandboxArgs. macOS (Seatbelt)
// and Linux (Landlock) sandboxes change no file permissions and stay on.
func CodexPolicy(spec runner.Spec) (sandbox, approval string) {
	plan := spec.PermissionMode == "plan"
	switch {
	case spec.CodexFullAccess && !plan:
		return "", "never"
	case codexGOOS == "windows" && !spec.CodexWindowsSandbox:
		if plan {
			return "danger-full-access", "untrusted"
		}
		return "danger-full-access", "on-request"
	case plan:
		return "read-only", "on-request"
	}
	return "workspace-write", "on-request"
}

// CodexLaunchMarker is the line that ties a new Codex conversation to the launch that made
// it (a folder and a time are not an identity: simultaneous agents share both).
func CodexLaunchMarker(id string) string { return "[tmx launch binding: " + id + "]" }

// tomlString: a JSON string is a valid TOML basic string.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func envValues(env, keys []string) map[string]string {
	out := map[string]string{}
	for _, k := range keys {
		for _, kv := range env {
			if strings.HasPrefix(kv, k+"=") {
				out[k] = kv[len(k)+1:]
			}
		}
	}
	return out
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewPiSession returns a fresh pi conversation id (assigned up front, like Claude's).
func NewPiSession() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewOpenCodeSession returns a fresh opencode conversation id. opencode takes the id at
// launch ("session to continue, or to create if it does not exist") and requires the
// "ses" prefix; it accepts any suffix, so tmx can know a new conversation's id up front.
func NewOpenCodeSession() string {
	var b [13]byte
	_, _ = rand.Read(b[:])
	return "ses_" + hex.EncodeToString(b[:])
}

// TrustableFolder reports whether tmx may answer a folder-trust dialog for dir on the
// owner's behalf: an existing folder that is not a drive root, a system folder, the home
// folder or one of its direct children (Documents, Desktop, ...). Anywhere else the dialog
// waits for a human.
func TrustableFolder(dir string) bool {
	if dir == "" {
		return false
	}
	p, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return false
	}
	if filepath.Dir(p) == p { // a root
		return false
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		if h, err := filepath.EvalSymlinks(home); err == nil {
			home = h
		}
		if samePath(p, home) || samePath(filepath.Dir(p), home) {
			return false
		}
	}
	for _, sys := range systemDirs() {
		if samePath(p, sys) || strings.HasPrefix(strings.ToLower(p)+string(filepath.Separator), strings.ToLower(sys)+string(filepath.Separator)) {
			return false
		}
	}
	return true
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func systemDirs() []string {
	if runtime.GOOS == "windows" {
		return []string{os.Getenv("SystemRoot"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramData")}
	}
	return []string{"/System", "/Library", "/usr", "/bin", "/sbin", "/etc", "/var", "/private/etc", "/private/var/db", "/Applications", "/opt", "/boot", "/dev", "/proc", "/sys"}
}

// CodexAppServerArgs is the argv (without the executable) of chat mode: `codex app-server`
// speaking JSON-RPC on stdio. Model, sandbox, approvals and instructions go in thread/start.
func CodexAppServerArgs(spec runner.Spec) []string {
	args := []string{"app-server", "-c", "check_for_update_on_startup=false"}
	if spec.CodexGuardHome != "" {
		args = append(args, "--strict-config")
	}
	sandbox, approval := CodexPolicy(spec)
	if sandbox == "" {
		sandbox = "danger-full-access"
	}
	args = append(args, "-c", "sandbox_mode="+tomlString(sandbox), "-c", "approval_policy="+tomlString(approval))
	args = append(args, CodexSandboxArgs(spec)...)
	args = append(args, codexMCPArgs(spec.MCP, spec.CodexGuardHome == "")...)
	// The support installer explicitly authorizes chat on the assigned owner DM.
	// Go MCP still enforces agent/chat membership; no command approval changes.
	if spec.Name == "support-agent" && spec.CodexGuardHome != "" && spec.MCP != nil && !spec.CodexFullAccess {
		args = append(args,
			"-c", `mcp_servers.tmxg.tools.chat_read.approval_mode="approve"`,
			"-c", `mcp_servers.tmxg.tools.chat_send.approval_mode="approve"`)
	}
	return append(args, spec.ExtraArgs...)
}
