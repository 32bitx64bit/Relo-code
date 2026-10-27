package art

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCostUSDFollowsTheReportedUsage(t *testing.T) {
	// 1000 text and 2000 image tokens in, 4000 out, at $5, $8 and $30 per million.
	got := CostUSD("gpt-image-2.5-flare", Usage{TextIn: 1000, ImageIn: 2000, ImageOut: 4000})
	if want := 0.005 + 0.016 + 0.12; !near(got, want) {
		t.Fatalf("cost = %v, want %v", got, want)
	}
	if mini := CostUSD("gpt-image-1-mini", Usage{ImageOut: 1_000_000}); !near(mini, 8) {
		t.Fatalf("mini output = %v", mini)
	}
	// A model the table has not heard of is priced at the highest rate, never lower.
	if unknown := CostUSD("some-new-model", Usage{ImageOut: 1_000_000}); unknown < 30 {
		t.Fatalf("unknown model cost %v is below a known one", unknown)
	}
}

func TestEstimateIsHighAndGrowsWithWhatYouAskFor(t *testing.T) {
	one := EstimateUSD(DefaultModel, "medium", "1024x1024", 1, 0, 100)
	if one < 0.053 || one > 0.06 {
		t.Fatalf("one medium square = %v", one)
	}
	if two := EstimateUSD(DefaultModel, "medium", "1024x1024", 2, 0, 100); two < 2*0.053 {
		t.Fatalf("two pictures = %v", two)
	}
	if withRefs := EstimateUSD(DefaultModel, "medium", "1024x1024", 1, 3, 100); withRefs <= one {
		t.Fatalf("references should add to the price: %v vs %v", withRefs, one)
	}
	if low, high := EstimateUSD(DefaultModel, "low", "1024x1024", 1, 0, 0), EstimateUSD(DefaultModel, "high", "1024x1024", 1, 0, 0); low >= one || high <= one {
		t.Fatalf("quality ordering: %v %v %v", low, one, high)
	}
	if wide := EstimateUSD(DefaultModel, "high", "1536x1024", 1, 0, 0); wide >= EstimateUSD(DefaultModel, "high", "1024x1024", 1, 0, 0) {
		t.Fatalf("a wide picture is listed cheaper than a square one, got %v", wide)
	}
	if auto, medium := EstimateUSD(DefaultModel, "auto", "auto", 1, 0, 0), EstimateUSD(DefaultModel, "medium", "1024x1024", 1, 0, 0); !near(auto, medium) {
		t.Fatalf("auto should price as a medium square: %v vs %v", auto, medium)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string][3]int{"1024x1024": {1024, 1024, 1}, "1536X1024": {1536, 1024, 1}, "auto": {0, 0, 0}, "x5": {0, 0, 0}, "10x-5": {0, 0, 0}} {
		w, h, ok := parseSize(in)
		if ok != (want[2] == 1) || w != want[0] || h != want[1] {
			t.Errorf("parseSize(%q) = %d %d %v", in, w, h, ok)
		}
	}
}
