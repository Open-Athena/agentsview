package pricing

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const litellmURL = "https://raw.githubusercontent.com/" +
	"BerriAI/litellm/main/" +
	"model_prices_and_context_window.json"

// ModelPricing holds per-model token pricing in cost per
// million tokens. Separate from db.ModelPricing — the CLI
// command converts between the two.
//
// CacheCreationPerMTok is the 5-minute ephemeral cache write rate
// (Anthropic: 1.25x input). CacheCreation1hPerMTok is the 1-hour
// ephemeral cache write rate (Anthropic: 2x input, ~1.6x the 5m
// rate). LiteLLM's catalog currently exposes only the 5m rate;
// 1h is derived as 1.6x the 5m rate when populated from LiteLLM,
// or set explicitly for fallback entries.
type ModelPricing struct {
	ModelPattern           string
	InputPerMTok           float64
	OutputPerMTok          float64
	CacheCreationPerMTok   float64
	CacheCreation1hPerMTok float64
	CacheReadPerMTok       float64
}

// cache1hRatio is the Anthropic-documented ratio of 1h cache write
// rate to 5m cache write rate (1h = 2x input, 5m = 1.25x input).
// Used to synthesize a 1h rate from upstream catalogs (LiteLLM)
// that only expose the 5m rate.
const cache1hRatio = 2.0 / 1.25

type litellmEntry struct {
	InputCost         *float64 `json:"input_cost_per_token"`
	OutputCost        *float64 `json:"output_cost_per_token"`
	CacheCreationCost *float64 `json:"cache_creation_input_token_cost"`
	CacheReadCost     *float64 `json:"cache_read_input_token_cost"`
	Provider          string   `json:"litellm_provider"`
}

const perMTok = 1_000_000

// FetchLiteLLMPricing downloads the LiteLLM pricing JSON
// and parses it into ModelPricing entries.
func FetchLiteLLMPricing() ([]ModelPricing, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(litellmURL)
	if err != nil {
		return nil, fmt.Errorf("fetching litellm pricing: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"fetching litellm pricing: status %d", resp.StatusCode,
		)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading litellm response: %w", err)
	}

	return ParseLiteLLMPricing(data)
}

// ParseLiteLLMPricing parses the LiteLLM JSON map into
// ModelPricing entries. Per-token costs are converted to
// per-million-token costs. Entries missing both input and
// output cost are skipped.
func ParseLiteLLMPricing(
	data []byte,
) ([]ModelPricing, error) {
	var raw map[string]litellmEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing litellm JSON: %w", err)
	}

	var prices []ModelPricing
	for model, entry := range raw {
		if entry.InputCost == nil && entry.OutputCost == nil {
			continue
		}
		p := ModelPricing{ModelPattern: model}
		if entry.InputCost != nil {
			p.InputPerMTok = *entry.InputCost * perMTok
		}
		if entry.OutputCost != nil {
			p.OutputPerMTok = *entry.OutputCost * perMTok
		}
		if entry.CacheCreationCost != nil {
			p.CacheCreationPerMTok =
				*entry.CacheCreationCost * perMTok
			p.CacheCreation1hPerMTok =
				p.CacheCreationPerMTok * cache1hRatio
		}
		if entry.CacheReadCost != nil {
			p.CacheReadPerMTok = *entry.CacheReadCost * perMTok
		}
		prices = append(prices, p)
	}
	return prices, nil
}
