package sessioncfg

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func generate(t *testing.T, profile, resume string) (Files, map[string]any, []string) {
	t.Helper()
	files, err := Generate(Input{TaskID: "t-abcdefgh", Profile: profile, Prompt: "do the thing", ClaudeSessionID: resume, AejardURL: "http://aejard:9111"})
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(files["settings.json"], &settings); err != nil {
		t.Fatal(err)
	}
	var launch struct{ Args []string }
	if err := json.Unmarshal(files["launch.json"], &launch); err != nil {
		t.Fatal(err)
	}
	return files, settings, launch.Args
}

func TestPreToolUseHookFailsClosed(t *testing.T) {
	files, settings, _ := generate(t, "plan", "")
	pre := settings["hooks"].(map[string]any)["PreToolUse"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if pre["onFailure"] != "block" || pre["command"] != "node "+HookPostPath+" pre-tool" {
		t.Fatalf("PreToolUse hook: %v", pre)
	}
	// The session token reaches hook-post through the environment only.
	if strings.Contains(string(files["settings.json"]), "Bearer") {
		t.Fatal("settings carry credentials")
	}
}

func TestConnectorsOnlyForMailProfile(t *testing.T) {
	_, plan, planArgs := generate(t, "plan", "")
	if plan["disableClaudeAiConnectors"] != true || !slices.Contains(planArgs, "--strict-mcp-config") {
		t.Fatal("plan profile must not load claude.ai connectors")
	}
	_, mail, mailArgs := generate(t, "mail", "")
	if _, set := mail["disableClaudeAiConnectors"]; set || slices.Contains(mailArgs, "--strict-mcp-config") {
		t.Fatal("mail profile needs the Gmail connector")
	}
}

func TestPermissionModes(t *testing.T) {
	_, _, planArgs := generate(t, "plan", "")
	if !slices.Contains(planArgs, "--dangerously-skip-permissions") {
		t.Fatal("plan sessions run unattended in the container")
	}
	_, action, actionArgs := generate(t, "action", "")
	if slices.Contains(actionArgs, "--dangerously-skip-permissions") {
		t.Fatal("action sessions must not skip permissions")
	}
	if action["permissions"].(map[string]any)["defaultMode"] != "dontAsk" {
		t.Fatal("action sessions deny anything not pre-approved")
	}
}

func TestResumeReplacesPrompt(t *testing.T) {
	_, _, fresh := generate(t, "plan", "")
	if fresh[len(fresh)-1] != "do the thing" {
		t.Fatalf("fresh session args end with %q", fresh[len(fresh)-1])
	}
	_, _, resumed := generate(t, "plan", "0b6a2c1e-1111-2222-3333-444455556666")
	i := slices.Index(resumed, "--resume")
	if i < 0 || resumed[i+1] != "0b6a2c1e-1111-2222-3333-444455556666" || slices.Contains(resumed, "do the thing") {
		t.Fatalf("resumed args: %v", resumed)
	}
}

func TestChannelLoaded(t *testing.T) {
	files, _, args := generate(t, "research", "")
	i := slices.Index(args, "--dangerously-load-development-channels")
	if i < 0 || args[i+1] != "server:aejar" {
		t.Fatalf("channel flag missing: %v", args)
	}
	if !strings.Contains(string(files["mcp.json"]), ChannelSocket) {
		t.Fatal("mcp config does not point the server at the channel socket")
	}
}

func TestUnknownProfile(t *testing.T) {
	if _, err := Generate(Input{Profile: "root"}); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

// Regression: Claude Code refuses HTTP hooks to private addresses (aejard is
// on a docker network), so every hook, the PreToolUse gate included, was
// silently dropped. All hooks must be command hooks via hook-post.
func TestHooksAreCommandHooks(t *testing.T) {
	_, settings, _ := generate(t, "mail", "")
	hooks := settings["hooks"].(map[string]any)
	for _, ev := range []string{"PreToolUse", "PostToolUse", "Stop", "StopFailure", "Notification", "SessionStart", "SessionEnd"} {
		entries, ok := hooks[ev].([]any)
		if !ok {
			t.Fatalf("no %s hook", ev)
		}
		h := entries[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
		if h["type"] != "command" || !strings.HasPrefix(h["command"].(string), "node "+HookPostPath+" ") {
			t.Errorf("%s hook: %v", ev, h)
		}
		if ev == "PreToolUse" && h["onFailure"] != "block" {
			t.Error("PreToolUse must fail closed")
		}
	}
}

func TestModelPerProfile(t *testing.T) {
	model := func(profile, override string) string {
		files, err := Generate(Input{TaskID: "t-abcdefgh", Profile: profile, Model: override, AejardURL: "http://aejard:9111"})
		if err != nil {
			t.Fatal(err)
		}
		var launch struct{ Args []string }
		json.Unmarshal(files["launch.json"], &launch)
		return launch.Args[slices.Index(launch.Args, "--model")+1]
	}
	if m := model("plan", ""); m != "sonnet" {
		t.Errorf("plan default: %s", m)
	}
	if m := model("project", ""); m != "opus" {
		t.Errorf("project default: %s", m)
	}
	if m := model("plan", "opus"); m != "opus" {
		t.Errorf("override: %s", m)
	}
}
