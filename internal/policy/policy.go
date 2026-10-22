// Package policy classifies Claude Code tool calls into risk tiers and decides
// what the PreToolUse hook answers. It is pure: no I/O, no clock.
//
// The gate lives here, in aejar, not in the model: whatever a session was
// told, a call the policy denies does not run. See integrations.html §2.
package policy

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Tier mirrors aejar.v1.Tier.
type Tier int

const (
	TierUnknown Tier = iota
	R0
	R1
	R2
	R3
	R4
)

func (t Tier) String() string {
	return [...]string{"unknown", "R0", "R1", "R2", "R3", "R4"}[t]
}

// Profile is a session profile. It decides which claude.ai connectors a
// session may use at all.
type Profile struct {
	Name string
	// Connectors lists claude.ai connector server names (as in the tool
	// name mcp__claude_ai_<Name>__op) this profile may call.
	Connectors []string
}

// Profiles are the built-in session profiles.
var Profiles = map[string]Profile{
	"plan":     {Name: "plan"},
	"research": {Name: "research"},
	"project":  {Name: "project"},
	"action":   {Name: "action"},
	"mail":     {Name: "mail", Connectors: []string{"Gmail"}},
}

// Classification is the result of classifying one call.
type Classification struct {
	Tier Tier
	// Sensitive means the call reads private data; the session enters
	// sensitive mode and loses web access.
	Sensitive bool
	// Web means the call reaches the public internet.
	Web    bool
	Reason string
}

// Verdict is what the hook does with a call.
type Verdict int

const (
	// Pass lets Claude Code's normal permission flow continue.
	Pass Verdict = iota
	// Deny blocks the call.
	Deny
	// NeedsApproval blocks the call unless a matching allow_once approval exists.
	NeedsApproval
)

// Decision is the policy's answer for one call.
type Decision struct {
	Verdict        Verdict
	Classification Classification
	Reason         string
}

// Input is everything the policy looks at.
type Input struct {
	Profile   string
	Sensitive bool // session already read sensitive data
	ToolName  string
	ToolInput map[string]any
}

// Config holds tunables.
type Config struct {
	// InternalHosts are hosts sessions may write to (aejard, coherence).
	InternalHosts []string
	// WritableRoots are path prefixes the file tools may write under.
	WritableRoots []string
}

// DefaultConfig matches the compose layout in implementation.html §13.
func DefaultConfig() Config {
	return Config{
		InternalHosts: []string{"aejard", "coherence", "localhost", "127.0.0.1"},
		WritableRoots: []string{"/work/", "/tmp/"},
	}
}

const approvalHint = "This action needs the owner's approval. Call the aejar request_approval tool with this exact tool name and input, then end your turn; do not retry or work around this."

// Decide classifies a call and applies profile and sensitive-mode rules.
func Decide(cfg Config, in Input) Decision {
	c := Classify(cfg, in.ToolName, in.ToolInput)
	d := Decision{Classification: c}

	if prof, ok := Profiles[in.Profile]; !ok {
		d.Verdict, d.Reason = Deny, fmt.Sprintf("unknown session profile %q", in.Profile)
		return d
	} else if server, isConnector := connectorServer(in.ToolName); isConnector && !contains(prof.Connectors, server) {
		d.Verdict, d.Reason = Deny, fmt.Sprintf("connector %q is not allowed in the %s profile", server, prof.Name)
		return d
	}

	switch c.Tier {
	case TierUnknown:
		d.Verdict, d.Reason = Deny, "unclassified tool "+in.ToolName+"; the owner must classify it before use ("+c.Reason+")"
		return d
	case R4:
		d.Verdict, d.Reason = Deny, "forbidden: "+c.Reason
		return d
	}
	if in.Sensitive && c.Web {
		d.Verdict, d.Reason = Deny, "this session has read private data, so it has no internet access for the rest of the session"
		return d
	}
	if c.Tier == R3 {
		d.Verdict, d.Reason = NeedsApproval, c.Reason+". "+approvalHint
		return d
	}
	d.Verdict = Pass
	return d
}

// readOnlyTools need no checks beyond the container boundary.
var readOnlyTools = map[string]bool{
	"Read": true, "Glob": true, "Grep": true, "LS": true, "NotebookRead": true,
	"TodoWrite": true, "Task": true, "Agent": true, "Skill": true, "ToolSearch": true,
	"ListAgents": true, "SendMessage": true, "TaskCreate": true, "TaskGet": true,
	"TaskList": true, "TaskUpdate": true, "TaskStop": true, "ExitPlanMode": true,
	"EnterPlanMode": true, "Monitor": true, "AskUserQuestion": true,
}

var fileWriteTools = map[string]string{"Edit": "file_path", "Write": "file_path", "NotebookEdit": "notebook_path"}

// Classify assigns a tier to a tool call.
func Classify(cfg Config, tool string, input map[string]any) Classification {
	switch {
	case readOnlyTools[tool]:
		return Classification{Tier: R0}
	case fileWriteTools[tool] != "":
		path, _ := input[fileWriteTools[tool]].(string)
		for _, root := range cfg.WritableRoots {
			if strings.HasPrefix(path, root) {
				return Classification{Tier: R0}
			}
		}
		return Classification{Tier: R4, Reason: "file write outside the workspace: " + path}
	case tool == "WebFetch" || tool == "WebSearch":
		return Classification{Tier: R1, Web: true}
	case tool == "Bash":
		cmd, _ := input["command"].(string)
		return classifyBash(cfg, cmd)
	case strings.HasPrefix(tool, "mcp__claude_ai_"):
		return classifyConnector(tool)
	case strings.HasPrefix(tool, "mcp__aejar__"):
		// aejar's own tools enforce their tiers server-side; calling them
		// is always allowed.
		return Classification{Tier: R0}
	}
	return Classification{Tier: TierUnknown, Reason: "no rule for this tool"}
}

var (
	// Commands that reach other machines or the host's control plane.
	forbiddenCmd = regexp.MustCompile(`(^|[\s;&|(])(ssh|scp|sftp|rsync|docker|kubectl|sudo|su|nc|ncat|netcat|telnet|sendmail|mail|mutt|msmtp|swaks)(\s|$)`)
	httpClient   = regexp.MustCompile(`(^|[\s;&|(])(curl|wget|http|https|xh)(\s|$)`)
	// Flags that make curl/wget send data or use a non-GET method.
	writeFlag = regexp.MustCompile(`(\s-X\s*(POST|PUT|PATCH|DELETE)|\s--request\s+(POST|PUT|PATCH|DELETE)|\s-d\b|\s--data\S*|\s-F\b|\s--form\b|\s-T\b|\s--upload-file\b|\s--post-data\b|\s--post-file\b|\s--method[= ](POST|PUT|PATCH|DELETE))`)
	urlRe     = regexp.MustCompile(`https?://[^\s'"]+`)
	gitPush   = regexp.MustCompile(`(^|[\s;&|(])git\s+(\S+\s+)*push(\s|$)`)
)

func classifyBash(cfg Config, cmd string) Classification {
	if m := forbiddenCmd.FindStringSubmatch(cmd); m != nil {
		return Classification{Tier: R4, Reason: "command " + m[2] + " is not available to sessions"}
	}
	if gitPush.MatchString(cmd) {
		return Classification{Tier: R4, Reason: "sessions do not push; use the aejar publish_branch tool"}
	}
	if !httpClient.MatchString(cmd) {
		return Classification{Tier: R0}
	}
	urls := urlRe.FindAllString(cmd, -1)
	internal := len(urls) > 0
	for _, u := range urls {
		if !isInternal(cfg, u) {
			internal = false
		}
	}
	if internal {
		return Classification{Tier: R0}
	}
	if writeFlag.MatchString(" " + cmd) {
		return Classification{Tier: R3, Web: true, Reason: "sending data to the internet changes something outside aejar"}
	}
	return Classification{Tier: R1, Web: true}
}

func isInternal(cfg Config, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return contains(cfg.InternalHosts, u.Hostname())
}

// connectorOps maps keywords in a connector operation name to a tier.
// The first matching rule wins, so destructive keywords come first.
var connectorOps = []struct {
	words     []string
	tier      Tier
	sensitive bool
	reason    string
}{
	{[]string{"delete_permanent", "permanently", "empty_trash"}, R4, false, "permanent deletion"},
	{[]string{"send", "reply", "forward", "trash", "delete", "share", "invite"}, R3, false, "this sends or changes something other people see or that cannot be undone"},
	{[]string{"draft", "label", "archive", "mark"}, R2, true, ""},
	{[]string{"search", "read", "get", "list", "fetch", "query"}, R1, true, ""},
}

func classifyConnector(tool string) Classification {
	server, _ := connectorServer(tool)
	op := strings.ToLower(tool[strings.LastIndex(tool, "__")+2:])
	if op == "authenticate" || op == "complete_authentication" {
		return Classification{Tier: R4, Reason: "connector authentication is done by the owner, not by sessions"}
	}
	for _, rule := range connectorOps {
		for _, w := range rule.words {
			if strings.Contains(op, w) {
				return Classification{Tier: rule.tier, Sensitive: rule.sensitive, Reason: rule.reason}
			}
		}
	}
	return Classification{Tier: TierUnknown, Reason: "connector " + server + " operation " + op}
}

// connectorServer extracts <Name> from mcp__claude_ai_<Name>__op.
func connectorServer(tool string) (string, bool) {
	rest, ok := strings.CutPrefix(tool, "mcp__claude_ai_")
	if !ok {
		return "", false
	}
	name, _, ok := strings.Cut(rest, "__")
	return name, ok
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
