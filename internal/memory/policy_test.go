package memory

import (
	"testing"
	"time"
)

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("") != 0 {
		t.Fatal("empty")
	}
	if n := EstimateTokens("abcd"); n != 1 {
		t.Fatalf("got %d", n)
	}
	if n := EstimateTokens("abcdefgh"); n != 2 {
		t.Fatalf("got %d", n)
	}
}

func TestDecayScore_halves(t *testing.T) {
	hl := 7 * 24 * time.Hour
	fresh := DecayScore(0, 0, hl)
	half := DecayScore(hl, 0, hl)
	if fresh < 0.99 || fresh > 1.01 {
		t.Fatalf("fresh=%v", fresh)
	}
	if half < 0.45 || half > 0.55 {
		t.Fatalf("half=%v want ~0.5", half)
	}
	boosted := DecayScore(hl, 10, hl)
	if boosted <= half {
		t.Fatal("hits should boost score")
	}
}

func TestSelectKept_ttlAndCap(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	p := Policy{TTL: 24 * time.Hour, HalfLife: 24 * time.Hour, MaxEntries: 2, MinScore: 0}
	items := []Item{
		{ID: "old", LastAccess: now.Add(-48 * time.Hour), Hits: 0},
		{ID: "a", LastAccess: now.Add(-1 * time.Hour), Hits: 0},
		{ID: "b", LastAccess: now.Add(-2 * time.Hour), Hits: 5},
		{ID: "c", LastAccess: now.Add(-3 * time.Hour), Hits: 0},
	}
	got := SelectKept(items, now, p)
	if len(got) != 2 {
		t.Fatalf("len=%d %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, g := range got {
		ids[g.ID] = true
	}
	if ids["old"] {
		t.Fatal("expired should drop")
	}
	if !ids["b"] {
		t.Fatal("high hits should survive cap")
	}
}

func TestSelectKept_minScoreDropsNoise(t *testing.T) {
	now := time.Now()
	p := Policy{TTL: 720 * time.Hour, HalfLife: time.Hour, MaxEntries: 10, MinScore: 0.9}
	got := SelectKept([]Item{
		{ID: "noise", LastAccess: now.Add(-10 * time.Hour), Hits: 0},
		{ID: "hot", LastAccess: now, Hits: 3},
	}, now, p)
	if len(got) != 1 || got[0].ID != "hot" {
		t.Fatalf("got %+v", got)
	}
}
