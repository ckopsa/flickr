package pipeline

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestIdleSessionIDs(t *testing.T) {
	now := time.Now()
	access := map[string]time.Time{
		"fresh":    now.Add(-30 * time.Second),
		"borderly": now.Add(-5 * time.Minute), // exactly at the limit: kept
		"stale":    now.Add(-5*time.Minute - time.Second),
		"ancient":  now.Add(-2 * time.Hour),
	}
	got := idleSessionIDs(access, now, 5*time.Minute)
	slices.Sort(got)
	want := []string{"ancient", "stale"}
	if !slices.Equal(got, want) {
		t.Errorf("idleSessionIDs = %v, want %v", got, want)
	}
}

func TestIdleSessionIDsEmpty(t *testing.T) {
	if got := idleSessionIDs(map[string]time.Time{}, time.Now(), time.Minute); len(got) != 0 {
		t.Errorf("expected no ids, got %v", got)
	}
}

// Touch on an unknown id must not resurrect a stopped session's access entry.
func TestTouchUnknownSessionIsNoop(t *testing.T) {
	m := NewSessionManager(t.TempDir(), nil)
	m.Touch("nope")
	if len(m.lastAccess) != 0 {
		t.Errorf("Touch on unknown id created an entry: %v", m.lastAccess)
	}
}

func TestSubtitleArgs(t *testing.T) {
	s := strings.Join(SubtitleArgs("http://x/in.mkv", 2, "/subs/7-2.vtt"), " ")
	for _, want := range []string{"-map 0:s:2", "-f webvtt", "-i http://x/in.mkv", "/subs/7-2.vtt"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
}
