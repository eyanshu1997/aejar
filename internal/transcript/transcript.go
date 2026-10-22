// Package transcript reads Claude Code's session transcripts (.jsonl) into a
// readable conversation: the prompts and replies, each tool call and its
// result. Internal records (thinking, API bookkeeping) are skipped.
package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one line of the conversation.
type Entry struct {
	TS   time.Time
	Role string // user, assistant
	Kind string // text, tool_use, tool_result
	Text string
	Tool string
	uuid string
}

type line struct {
	Type      string          `json:"type"`
	UUID      string          `json:"uuid"`
	Timestamp time.Time       `json:"timestamp"`
	IsMeta    bool            `json:"isMeta"`
	Message   json.RawMessage `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

const maxText = 4000

// Parse reads one transcript file.
func Parse(r io.Reader) ([]Entry, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var out []Entry
	for sc.Scan() {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil || (l.Type != "user" && l.Type != "assistant") || l.IsMeta {
			continue
		}
		var m message
		if json.Unmarshal(l.Message, &m) != nil {
			continue
		}
		add := func(kind, text, tool string, n int) {
			text = strings.TrimSpace(text)
			if text == "" && kind == "text" {
				return
			}
			out = append(out, Entry{TS: l.Timestamp, Role: l.Type, Kind: kind, Text: clip(text), Tool: tool,
				uuid: l.UUID + "/" + string(rune('a'+n))})
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			add("text", s, "", 0)
			continue
		}
		var blocks []block
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for i, b := range blocks {
			switch b.Type {
			case "text":
				add("text", b.Text, "", i)
			case "tool_use":
				add("tool_use", string(b.Input), b.Name, i)
			case "tool_result":
				text := resultText(b.Content)
				if b.IsError {
					text = "error: " + text
				}
				add("tool_result", text, "", i)
			}
		}
	}
	return out, sc.Err()
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []block
	if json.Unmarshal(raw, &parts) == nil {
		var texts []string
		for _, p := range parts {
			if p.Type == "text" {
				texts = append(texts, p.Text)
			} else if p.Type != "" {
				texts = append(texts, "["+p.Type+"]")
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(raw)
}

func clip(s string) string {
	if len(s) <= maxText {
		return s
	}
	return s[:maxText] + "\n… (" + itoa(len(s)-maxText) + " more characters)"
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// ReadDir reads every transcript under a session home's .claude/projects, in
// time order, without duplicates (a resumed session can repeat history).
func ReadDir(home string) ([]Entry, error) {
	files, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	sort.Slice(files, func(i, j int) bool {
		a, _ := os.Stat(files[i])
		b, _ := os.Stat(files[j])
		return a != nil && b != nil && a.ModTime().Before(b.ModTime())
	})
	seen := map[string]bool{}
	var all []Entry
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		entries, err := Parse(fh)
		fh.Close()
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !seen[e.uuid] {
				seen[e.uuid] = true
				all = append(all, e)
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS.Before(all[j].TS) })
	return all, nil
}
