package scheduler

import (
	"reflect"
	"testing"
	"time"
)

var (
	noon = time.Date(2026, 10, 22, 12, 0, 0, 0, DefaultConfig().Location)
	ok   = Resources{MemAvailableMB: 2000, Load1: 1}
)

func task(id string, c Class, s State, ageMin int) Task {
	return Task{ID: id, Class: c, State: s, CreatedAt: noon.Add(-time.Duration(ageMin) * time.Minute)}
}

func TestStartsHighestScoreFirstWithinCap(t *testing.T) {
	cfg := DefaultConfig()
	tasks := []Task{task("p3", P3, Queued, 0), task("p1", P1, Queued, 0), task("p1old", P1, Queued, 100)}
	got := Decide(cfg, tasks, ok, noon)
	if want := []string{"p1old", "p1"}; !reflect.DeepEqual(got.Start, want) {
		t.Fatalf("start = %v, want %v", got.Start, want)
	}
}

func TestP0UsesReserveSlot(t *testing.T) {
	cfg := DefaultConfig()
	tasks := []Task{task("a", P1, Running, 0), task("b", P1, Running, 0), task("now", P0, Queued, 0), task("later", P1, Queued, 0)}
	got := Decide(cfg, tasks, ok, noon)
	if !reflect.DeepEqual(got.Start, []string{"now"}) || len(got.Preempt) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestP0PreemptsBackgroundWhenFull(t *testing.T) {
	cfg := DefaultConfig()
	tasks := []Task{
		task("a", P1, Running, 0), task("bg", P2, Running, 0), task("cron", P3, Running, 0),
		task("now", P0, Queued, 0),
	}
	got := Decide(cfg, tasks, ok, noon)
	if !reflect.DeepEqual(got.Preempt, []string{"cron"}) {
		t.Fatalf("preempt = %v, want [cron]", got.Preempt)
	}
}

func TestP1NeverPreempted(t *testing.T) {
	cfg := DefaultConfig()
	tasks := []Task{task("a", P1, Running, 0), task("b", P1, Running, 0), task("c", P1, Running, 0), task("now", P0, Queued, 0)}
	if got := Decide(cfg, tasks, ok, noon); len(got.Preempt) != 0 || len(got.Start) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestWaitingTasksHoldNoSlot(t *testing.T) {
	cfg := DefaultConfig()
	tasks := []Task{task("w1", P1, Other, 0), task("w2", P1, Other, 0), task("q", P1, Queued, 0)}
	if got := Decide(cfg, tasks, ok, noon); !reflect.DeepEqual(got.Start, []string{"q"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestGuards(t *testing.T) {
	cfg := DefaultConfig()
	tasks := []Task{task("q", P0, Queued, 0)}
	cases := map[string]Resources{
		"usage limit":     {MemAvailableMB: 2000, LimitBlockedUntil: noon.Add(time.Hour)},
		"low memory":      {MemAvailableMB: 300},
		"memory pressure": {MemAvailableMB: 2000, MemPressure: 40},
		"cpu load":        {MemAvailableMB: 2000, Load1: 3.9},
	}
	for name, res := range cases {
		got := Decide(cfg, tasks, res, noon)
		if len(got.Start) != 0 || got.Blocked == "" {
			t.Errorf("%s: got %+v", name, got)
		}
	}
}

func TestWakeBoostAndNightWindow(t *testing.T) {
	cfg := DefaultConfig()
	night := time.Date(2026, 10, 22, 3, 0, 0, 0, cfg.Location)
	bg := Task{Class: P2, CreatedAt: night}
	if Score(cfg, bg, night) <= Score(cfg, bg, night.Add(9*time.Hour))-1000 {
		t.Fatal("night window should favour P2")
	}
	woke := Task{Class: P1, WakeBoost: 100, CreatedAt: noon}
	if Score(cfg, woke, noon) != 600 {
		t.Fatalf("score = %d, want 600", Score(cfg, woke, noon))
	}
}
