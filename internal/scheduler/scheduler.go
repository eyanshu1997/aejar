// Package scheduler decides which tasks run. It is pure: callers pass the
// current tasks and resources, and get back what to start and what to
// preempt. See plan.html §5.
package scheduler

import (
	"sort"
	"time"
)

// Class mirrors aejar.v1.TaskClass.
type Class int

const (
	ClassUnspecified Class = iota
	P0
	P1
	P2
	P3
)

// State mirrors the subset of aejar.v1.TaskState the scheduler reasons about.
type State int

const (
	Queued State = iota
	Running
	Other // waiting, paused, done, ... : holds no slot, not startable
)

// Task is the scheduler's view of a task.
type Task struct {
	ID        string
	Class     Class
	State     State
	UserBump  int
	WakeBoost int
	CreatedAt time.Time
}

// Resources is a snapshot of machine and account state.
type Resources struct {
	MemAvailableMB int
	// MemPressure is /proc/pressure/memory "some avg10".
	MemPressure float64
	// Load1 is the 1-minute load average.
	Load1             float64
	LimitBlockedUntil time.Time
}

// Config holds the tunables. Cap changes go through a policy approval.
type Config struct {
	Cap int
	// Reserve extra slots usable only by P0 tasks.
	Reserve        int
	MinMemMB       int
	MaxMemPressure float64
	MaxLoad1       float64
	// Location for the night window.
	Location *time.Location
}

// DefaultConfig matches plan.html §5 on the 4-core / 3.7 GB server.
func DefaultConfig() Config {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	return Config{Cap: 2, Reserve: 1, MinMemMB: 700, MaxMemPressure: 10, MaxLoad1: 3.5, Location: loc}
}

var classBase = map[Class]int{P0: 1000, P1: 500, P2: 200, P3: 100}

// Score is a task's priority at now. Higher runs first.
func Score(cfg Config, t Task, now time.Time) int {
	s := classBase[t.Class] + t.UserBump + t.WakeBoost
	age := int(now.Sub(t.CreatedAt).Minutes()) / 10
	if age > 300 {
		age = 300
	}
	if age > 0 {
		s += age
	}
	if t.Class == P2 {
		// Long-running work prefers the night, keeping daytime slots for
		// interactive tasks.
		if h := now.In(cfg.Location).Hour(); h >= 1 && h < 7 {
			s += 200
		} else {
			s -= 100
		}
	}
	return s
}

// Plan is what the caller should do now.
type Plan struct {
	Start   []string
	Preempt []string
	// Blocked explains why nothing could start, for status and logs.
	Blocked string
}

// Decide computes the plan.
func Decide(cfg Config, tasks []Task, res Resources, now time.Time) Plan {
	var queued, running []Task
	for _, t := range tasks {
		switch t.State {
		case Queued:
			queued = append(queued, t)
		case Running:
			running = append(running, t)
		}
	}
	var p Plan
	if len(queued) == 0 {
		return p
	}
	if now.Before(res.LimitBlockedUntil) {
		p.Blocked = "usage limit until " + res.LimitBlockedUntil.In(cfg.Location).Format("15:04")
		return p
	}
	if res.MemAvailableMB < cfg.MinMemMB {
		p.Blocked = "low memory"
		return p
	}
	if res.MemPressure > cfg.MaxMemPressure {
		p.Blocked = "memory pressure"
		return p
	}
	if res.Load1 > cfg.MaxLoad1 {
		p.Blocked = "cpu load"
		return p
	}

	byScore := func(ts []Task) {
		sort.SliceStable(ts, func(i, j int) bool {
			si, sj := Score(cfg, ts[i], now), Score(cfg, ts[j], now)
			if si != sj {
				return si > sj
			}
			return ts[i].CreatedAt.Before(ts[j].CreatedAt)
		})
	}
	byScore(queued)

	n := len(running)
	for _, t := range queued {
		limit := cfg.Cap
		if t.Class == P0 {
			limit += cfg.Reserve
		}
		if n < limit {
			p.Start = append(p.Start, t.ID)
			n++
			continue
		}
		if t.Class != P0 {
			continue
		}
		// A P0 task with no slot preempts the lowest-scored P2/P3 task.
		victim := lowestPreemptible(cfg, running, p.Preempt, now)
		if victim == "" {
			continue
		}
		p.Preempt = append(p.Preempt, victim)
	}
	if len(p.Start) == 0 && len(p.Preempt) == 0 {
		p.Blocked = "all slots busy"
	}
	return p
}

func lowestPreemptible(cfg Config, running []Task, already []string, now time.Time) string {
	best, bestScore, bestClass := "", 0, ClassUnspecified
	for _, t := range running {
		if t.Class != P2 && t.Class != P3 {
			continue
		}
		taken := false
		for _, id := range already {
			taken = taken || id == t.ID
		}
		if taken {
			continue
		}
		// Lowest score loses; on a tie the lower-priority class (P3) goes first.
		if s := Score(cfg, t, now); best == "" || s < bestScore || (s == bestScore && t.Class > bestClass) {
			best, bestScore, bestClass = t.ID, s, t.Class
		}
	}
	return best
}
