package pricing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFallbackPricing_Opus46Rates(t *testing.T) {
	prices := FallbackPricing()
	var got *ModelPricing
	for i := range prices {
		if prices[i].ModelPattern == "claude-opus-4-6" {
			got = &prices[i]
			break
		}
	}
	require.NotNil(t, got, "claude-opus-4-6 entry missing from FallbackPricing")

	// Source: https://claude.com/pricing — Opus 4.5/4.6 tier.
	// CacheCreation1hPerMTok = 2x input rate (Anthropic's 1h
	// ephemeral cache write is 2x input, vs 1.25x for 5m).
	want := ModelPricing{
		ModelPattern:           "claude-opus-4-6",
		InputPerMTok:           5.0,
		OutputPerMTok:          25.0,
		CacheCreationPerMTok:   6.25,
		CacheCreation1hPerMTok: 10.0,
		CacheReadPerMTok:       0.50,
	}
	assert.Equal(t, want, *got)
}

func TestFallbackPricing_Opus48Rates(t *testing.T) {
	prices := FallbackPricing()
	var got *ModelPricing
	for i := range prices {
		if prices[i].ModelPattern == "claude-opus-4-8" {
			got = &prices[i]
			break
		}
	}
	require.NotNil(t, got, "claude-opus-4-8 entry missing from FallbackPricing")

	// Opus 4.8 launched at the same rates as Opus 4.6/4.7 and is
	// not yet in the LiteLLM catalog, so the shipped fallback must
	// price it at the current Opus tier (incl. 1h cache rate).
	want := ModelPricing{
		ModelPattern:           "claude-opus-4-8",
		InputPerMTok:           5.0,
		OutputPerMTok:          25.0,
		CacheCreationPerMTok:   6.25,
		CacheCreation1hPerMTok: 10.0,
		CacheReadPerMTok:       0.50,
	}
	assert.Equal(t, want, *got)
}
