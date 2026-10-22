package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

const sample = `{"type":"user","uuid":"u1","timestamp":"2026-10-22T10:00:00Z","message":{"role":"user","content":"List the files"}}
{"type":"assistant","uuid":"a1","timestamp":"2026-10-22T10:00:01Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"secret reasoning"},{"type":"text","text":"Sure."},{"type":"tool_use","id":"x","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","uuid":"u2","timestamp":"2026-10-22T10:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":[{"type":"text","text":"a.go\nb.go"}]}]}}
{"type":"api-request-blob","uuid":"r1","message":{"content":"internal"}}
{"type":"user","uuid":"m1","isMeta":true,"timestamp":"2026-10-22T10:00:03Z","message":{"role":"user","content":"meta"}}
{"type":"assistant","uuid":"a2","timestamp":"2026-10-22T10:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"Two files."}]}}
`

func TestParse(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, ".claude", "projects", "-work")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "s1.jsonl"), []byte(sample), 0o644)
	// A resumed session repeats history in a second file.
	os.WriteFile(filepath.Join(proj, "s2.jsonl"), []byte(sample), 0o644)
	got, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ role, kind, text, tool string }{
		{"user", "text", "List the files", ""},
		{"assistant", "text", "Sure.", ""},
		{"assistant", "tool_use", `{"command":"ls"}`, "Bash"},
		{"user", "tool_result", "a.go\nb.go", ""},
		{"assistant", "text", "Two files.", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries: %+v", len(got), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Role != w.role || g.Kind != w.kind || g.Text != w.text || g.Tool != w.tool {
			t.Errorf("%d: got %+v want %+v", i, g, w)
		}
	}
}
