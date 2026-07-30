package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/money"
)

const litellmURL = "https://raw.githubusercontent.com/" +
	"BerriAI/litellm/main/" +
	"model_prices_and_context_window.json"

// ModelPricing holds per-model token pricing in cost per million tokens.
//
// CacheCreationPerMTok is the 5-minute ephemeral cache-write rate
// (Anthropic: 1.25x input). CacheCreation1hPerMTok is the 1-hour
// ephemeral cache-write rate (Anthropic: 2x input, ~1.6x the 5m rate).
// LiteLLM's catalog exposes only the 5m rate, so 1h is derived as
// cache1hRatio x the 5m rate; the bundled snapshot overlay sets explicit
// 1h values for Anthropic models.
type ModelPricing struct {
	ModelPattern         string
	InputPerMTok         money.Money
	OutputPerMTok        money.Money
	CacheCreationPerMTok money.Money
	// CacheCreation1hPerMTok is the 1-hour ephemeral cache-write rate
	// (Anthropic: 2x input, exactly 8/5 of the 5m rate). LiteLLM exposes
	// only the 5m rate, so 1h is derived by Fill1hCacheRate; the bundled
	// snapshot overlay sets explicit 1h values for Anthropic models.
	CacheCreation1hPerMTok money.Money
	CacheReadPerMTok       money.Money
}

// Fill1hCacheRate derives the 1h ephemeral cache-write rate from the 5m rate
// for entries that carry a 5m rate but no explicit 1h rate. The Anthropic
// ratio is exactly 8/5 (1h = 2x input, 5m = 1.25x input), computed in money
// terms via multiply-by-8 then divide-by-5. Shared by the LiteLLM parser and
// the snapshot generator's overlay so both sources ship a 1h rate.
func Fill1hCacheRate(p *ModelPricing) {
	if p.CacheCreation1hPerMTok == (money.Money{}) &&
		p.CacheCreationPerMTok.Microdollars > 0 {
		eightX := money.Money{
			Microdollars: p.CacheCreationPerMTok.Microdollars * 8,
		}
		p.CacheCreation1hPerMTok, _ = money.Divide(eightX, 5)
	}
}

type litellmEntry struct {
	InputCost         *json.Number `json:"input_cost_per_token"`
	OutputCost        *json.Number `json:"output_cost_per_token"`
	CacheCreationCost *json.Number `json:"cache_creation_input_token_cost"`
	CacheReadCost     *json.Number `json:"cache_read_input_token_cost"`
	Provider          string       `json:"litellm_provider"`
}

// FetchLiteLLMPricing downloads the LiteLLM pricing JSON
// and parses it into ModelPricing entries.
func FetchLiteLLMPricing() ([]ModelPricing, error) {
	return FetchLiteLLMPricingContext(context.Background())
}

// FetchLiteLLMPricingContext downloads the LiteLLM pricing JSON and binds the
// request lifetime to ctx.
func FetchLiteLLMPricingContext(
	ctx context.Context,
) ([]ModelPricing, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return fetchLiteLLMPricing(ctx, client, litellmURL)
}

func fetchLiteLLMPricing(
	ctx context.Context,
	client *http.Client,
	url string,
) ([]ModelPricing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating litellm pricing request: %w", err)
	}
	resp, err := client.Do(req)
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
		var err error
		if entry.InputCost != nil {
			p.InputPerMTok, err = parsePerTokenRate(*entry.InputCost)
			if err != nil {
				return nil, fmt.Errorf("parsing %s input price: %w", model, err)
			}
		}
		if entry.OutputCost != nil {
			p.OutputPerMTok, err = parsePerTokenRate(*entry.OutputCost)
			if err != nil {
				return nil, fmt.Errorf("parsing %s output price: %w", model, err)
			}
		}
		if entry.CacheCreationCost != nil {
			p.CacheCreationPerMTok, err = parsePerTokenRate(*entry.CacheCreationCost)
			if err != nil {
				return nil, fmt.Errorf("parsing %s cache creation price: %w", model, err)
			}
		}
		if entry.CacheReadCost != nil {
			p.CacheReadPerMTok, err = parsePerTokenRate(*entry.CacheReadCost)
			if err != nil {
				return nil, fmt.Errorf("parsing %s cache read price: %w", model, err)
			}
		}
		Fill1hCacheRate(&p)
		prices = append(prices, p)
	}
	return prices, nil
}

func parsePerTokenRate(value json.Number) (money.Money, error) {
	microdollars, err := money.ParseScaledDecimal(value.String(), 12)
	if err != nil {
		return money.Money{}, err
	}
	if microdollars < 0 {
		return money.Money{}, money.ErrNegative
	}
	return money.Money{Microdollars: microdollars}, nil
}
