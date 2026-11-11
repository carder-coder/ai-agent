package cost

import (
	"testing"

	"github.com/carder-coder/ai-agent/gateway/internal/provider"
)

func TestCalculateChargesRegularCachedAndOutputTokens(t *testing.T) {
	usage := provider.Usage{
		InputTokens:  10_000,
		OutputTokens: 5_000,
		TotalTokens:  15_000,
		CachedTokens: 8_000,
	}
	got, err := Calculate(usage, Prices{Input: 0.8, Output: 3.2, Cached: 0.016})
	if err != nil {
		t.Fatalf("Calculate() error = %v", err)
	}
	if got != 0.0177 {
		t.Fatalf("Calculate() = %.4f, want 0.0177", got)
	}
}

func TestCalculateRoundsHalfUpToFourDecimalPlaces(t *testing.T) {
	got, err := Calculate(
		provider.Usage{InputTokens: 63, TotalTokens: 63},
		Prices{Input: 0.8},
	)
	if err != nil {
		t.Fatalf("Calculate() error = %v", err)
	}
	if got != 0.0001 {
		t.Fatalf("Calculate() = %.4f, want 0.0001", got)
	}
}

func TestCalculateRejectsInvalidUsage(t *testing.T) {
	for _, usage := range []provider.Usage{
		{InputTokens: -1},
		{InputTokens: 2, CachedTokens: 3},
	} {
		if _, err := Calculate(usage, Prices{}); err == nil {
			t.Errorf("Calculate(%+v) accepted invalid usage", usage)
		}
	}
}

func TestUpperBoundRoundsUpForBudgetReservation(t *testing.T) {
	got, err := UpperBound(
		provider.Usage{InputTokens: 1, TotalTokens: 1},
		Prices{Input: 0.8},
	)
	if err != nil {
		t.Fatalf("UpperBound() error = %v", err)
	}
	if got != 0.0001 {
		t.Fatalf("UpperBound() = %.4f, want 0.0001", got)
	}
}
