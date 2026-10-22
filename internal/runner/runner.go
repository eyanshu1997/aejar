// Package runner starts and stops Claude sessions for tasks. The docker
// implementation (via aejar-spawner) lands in the next pass; Recorder is used
// in tests and for running aejard without containers.
package runner

import (
	"context"
	"sync"
)

// Spec is what a runner needs to start a session.
type Spec struct {
	TaskID  string
	Profile string
	Prompt  string
	// ClaudeSessionID is set when resuming (claude --resume).
	ClaudeSessionID string
	Model           string
	// SessionToken authenticates the session's hooks and aejar tool calls.
	SessionToken string
}

// Event is a message pushed into a running session.
type Event struct {
	ID      string
	Kind    string // chat, comment, action_result
	Content string
}

// Runner starts and stops sessions and delivers events into them.
type Runner interface {
	Start(ctx context.Context, spec Spec) error
	// Stop ends the session at its next idle point; the conversation is
	// kept so it can be resumed.
	Stop(ctx context.Context, taskID, reason string) error
	// Deliver pushes an event through the session's channel. An error means
	// the channel is not reachable yet; the caller keeps the event queued.
	Deliver(ctx context.Context, taskID string, ev Event) error
	// Type is the fallback input path: it types text into the session's
	// terminal, used only when the channel never comes up.
	Type(ctx context.Context, taskID, text string) error
	// Screen returns the session terminal's text, read-only.
	Screen(ctx context.Context, taskID string, historyLines int) (string, error)
}

// Call is one recorded runner call.
type Call struct {
	Op     string // start, stop, deliver, type
	Spec   Spec
	TaskID string
	Reason string
	Event  Event
}

// Recorder records calls and does nothing else.
type Recorder struct {
	mu         sync.Mutex
	calls      []Call
	deliverErr error
	startErr   error
	screen     string
}

// SetStartErr, when set, makes Start fail.
func (r *Recorder) SetStartErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startErr = err
}

func (r *Recorder) Start(_ context.Context, spec Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return r.startErr
	}
	r.calls = append(r.calls, Call{Op: "start", Spec: spec, TaskID: spec.TaskID})
	return nil
}

func (r *Recorder) Stop(_ context.Context, taskID, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Op: "stop", TaskID: taskID, Reason: reason})
	return nil
}

// DeliverErr, when set, makes Deliver fail (channel not up yet).
func (r *Recorder) SetDeliverErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliverErr = err
}

func (r *Recorder) Deliver(_ context.Context, taskID string, ev Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deliverErr != nil {
		return r.deliverErr
	}
	r.calls = append(r.calls, Call{Op: "deliver", TaskID: taskID, Event: ev})
	return nil
}

func (r *Recorder) Type(_ context.Context, taskID, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Op: "type", TaskID: taskID, Event: Event{Content: text}})
	return nil
}

// SetScreen sets what Screen returns.
func (r *Recorder) SetScreen(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.screen = text
}

func (r *Recorder) Screen(_ context.Context, _ string, _ int) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.screen, nil
}

// Calls returns a copy of the recorded calls.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// Log is a Runner that only logs; aejard uses it until the docker runner
// lands, so the control plane can run and be exercised end to end.
type Log struct {
	Logf func(format string, args ...any)
}

func (l Log) Start(_ context.Context, spec Spec) error {
	l.Logf("runner: would start task %s (profile %s, resume %q)", spec.TaskID, spec.Profile, spec.ClaudeSessionID)
	return nil
}

func (l Log) Stop(_ context.Context, taskID, reason string) error {
	l.Logf("runner: would stop task %s: %s", taskID, reason)
	return nil
}

func (l Log) Deliver(_ context.Context, taskID string, ev Event) error {
	l.Logf("runner: would deliver %s event %s to task %s", ev.Kind, ev.ID, taskID)
	return nil
}

func (l Log) Screen(_ context.Context, taskID string, _ int) (string, error) {
	return "(no session runner configured)", nil
}

func (l Log) Type(_ context.Context, taskID, _ string) error {
	l.Logf("runner: would type into task %s", taskID)
	return nil
}
