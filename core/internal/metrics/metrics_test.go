package metrics

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestCacheStatsRatesAndSnapshot(t *testing.T) {
	var stats CacheStats
	stats.AddLabeled("a", 1, "m", "worker", "v:full|c:full", Usage{Input: 10, CacheRead: 80, CacheWrite: 10})
	stats.AddLabeled("a", 2, "m", "worker", "v:full|c:full", Usage{Input: 20, CacheRead: 70, CacheWrite: 10})
	stats.Add("b", 1, "n", Usage{Input: 20, CacheRead: 0, CacheWrite: 0})
	if got := stats.HitRate("a"); math.Abs(got-0.75) > 1e-9 {
		t.Fatalf("agent hit rate = %v", got)
	}
	if got := stats.SessionHitRate(); math.Abs(got-(150.0/220.0)) > 1e-9 {
		t.Fatalf("session hit rate = %v", got)
	}
	snapshot := stats.Snapshot()
	if len(snapshot.Agents) != 2 || snapshot.Session.Usage.CacheRead != 150 || snapshot.Agents[0].Role != "worker" || snapshot.Agents[0].Profile != "v:full|c:full" || snapshot.Agents[0].WeightedInput != 70 || snapshot.Session.WeightedInput != 90 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	body, err := json.Marshal(snapshot)
	if err != nil || !strings.Contains(string(body), `"role":"worker"`) || !strings.Contains(string(body), `"profile":"v:full|c:full"`) || !strings.Contains(string(body), `"weightedInput":90`) {
		t.Fatalf("snapshot JSON = %s, err=%v", body, err)
	}
	var empty CacheStats
	if empty.HitRate("missing") != 0 || empty.SessionHitRate() != 0 {
		t.Fatal("zero usage should yield zero rate")
	}
}

func TestWeightedInput(t *testing.T) {
	u := Usage{Input: 100, CacheWrite: 40, CacheRead: 300}
	if got := WeightedInput(u, 2, 0.5); got != 330 {
		t.Fatalf("weighted input = %v", got)
	}
	if got := WeightedInput(u, DefaultCacheWriteWeight, DefaultCacheReadWeight); got != 180 {
		t.Fatalf("default weighted input = %v", got)
	}
}
