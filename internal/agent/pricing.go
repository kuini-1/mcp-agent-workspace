package agent

import (
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
)

// Price is USD per million tokens.
type Price struct {
	Input     float64
	Output    float64
	CacheRead float64
}

// Anthropic list prices, first-party API, as of November 2026.
// Cache writes (5-minute TTL) cost 1.25x the input price.
var prices = map[string]Price{
	"claude-fable-5-2":  {Input: 8, Output: 40, CacheRead: 0.20},
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.20},
	"claude-opus-5":     {Input: 5, Output: 25, CacheRead: 0.50},
	"claude-opus-4-8":   {Input: 5, Output: 25, CacheRead: 0.50},
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.10},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheRead: 0.20},
	"claude-haiku-5-5":  {Input: 0.10, Output: 0.50, CacheRead: 0.01},
}

const cacheWriteMultiplier = 1.25

// Cost returns the USD cost of one response, priced by the model that
// actually served it (a fallback may differ from the one requested).
// ok is false for models without a known price.
func Cost(model string, u anthropic.BetaUsage) (usd float64, ok bool) {
	p, ok := prices[strings.TrimSpace(model)]
	if !ok {
		return 0, false
	}
	const m = 1_000_000.0
	usd = float64(u.InputTokens)*p.Input/m +
		float64(u.CacheCreationInputTokens)*p.Input*cacheWriteMultiplier/m +
		float64(u.CacheReadInputTokens)*p.CacheRead/m +
		float64(u.OutputTokens)*p.Output/m
	return usd, true
}

// contextTokens is how many tokens the model read for a request: the number
// compaction compares against its threshold.
func contextTokens(u anthropic.BetaUsage) int64 {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}
