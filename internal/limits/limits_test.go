package limits

import (
	"testing"
	"time"
)

func TestResetTime(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Date(2026, 10, 22, 14, 0, 0, 0, ist)
	cases := []struct {
		msg  string
		want time.Time
		ok   bool
	}{
		{"Claude usage limit reached. Your limit resets 6pm (Asia/Kolkata).", time.Date(2026, 10, 22, 18, 0, 0, 0, ist), true},
		{"limit reached, resets at 18:30", time.Date(2026, 10, 22, 18, 30, 0, 0, ist), true},
		{"resets 5:30am", time.Date(2026, 10, 23, 5, 30, 0, 0, ist), true},
		{"resets 12am (UTC)", time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), true},
		{"resets 1pm", time.Date(2026, 10, 23, 13, 0, 0, 0, ist), true},
		{"API Error: Rate limit reached", now.Add(FallbackWait), false},
	}
	for _, tc := range cases {
		got, ok := ResetTime(tc.msg, now, ist)
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Errorf("%q: got %v %v, want %v %v", tc.msg, got, ok, tc.want, tc.ok)
		}
	}
}
