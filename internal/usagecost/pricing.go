package usagecost

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// Price is an API-equivalent price in USD per one million tokens.
type Price struct {
	Input      float64 `json:"input"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Output     float64 `json:"output"`
}

// defaultPrices are matched by case-insensitive substring, longest key first.
// They are public API list-price estimates; override them with model-pricing.json.
var defaultPrices = map[string]Price{
	// Claude 5.x. Cache write is the 5-minute rate (1.25x input).
	"claude-fable":    {Input: 10, CacheRead: 0.25, CacheWrite: 12.5, Output: 50},
	"claude-opus-5-5": {Input: 4, CacheRead: 0.2, CacheWrite: 5, Output: 20},
	"claude-sonnet-5": {Input: 2, CacheRead: 0.2, CacheWrite: 2.5, Output: 10},
	"claude-haiku-5":  {Input: 0.1, CacheRead: 0.01, CacheWrite: 0.125, Output: 0.5},
	// Older generations; the family keys below cover 4.5+ Opus, 4.x Sonnet and 4.5 Haiku.
	"claude-opus-4-1":    {Input: 15, CacheRead: 1.5, CacheWrite: 18.75, Output: 75},
	"claude-opus-4-2025": {Input: 15, CacheRead: 1.5, CacheWrite: 18.75, Output: 75},
	"claude-opus":        {Input: 5, CacheRead: 0.5, CacheWrite: 6.25, Output: 25},
	"claude-sonnet":      {Input: 3, CacheRead: 0.3, CacheWrite: 3.75, Output: 15},
	"claude-haiku":       {Input: 1, CacheRead: 0.1, CacheWrite: 1.25, Output: 5},
	"gpt-":               {Input: 1.25, CacheRead: 0.125, Output: 10},
	"gemini-":            {Input: 1.25, CacheRead: 0.31, Output: 10},
}

// Pricer resolves a model name to a price.
type Pricer struct {
	entries []pricerEntry
}

type pricerEntry struct {
	key   string
	price Price
}

// NewPricer builds a pricer from the defaults plus overrides; overrides win.
func NewPricer(overrides map[string]Price) *Pricer {
	merged := make(map[string]Price, len(defaultPrices)+len(overrides))
	for key, price := range defaultPrices {
		merged[key] = price
	}
	for key, price := range overrides {
		if key = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), ".", "-"); key != "" {
			merged[key] = price
		}
	}
	p := &Pricer{}
	for key, price := range merged {
		p.entries = append(p.entries, pricerEntry{key: key, price: price})
	}
	// Longest key first so a specific model beats a family prefix.
	sort.Slice(p.entries, func(i, j int) bool {
		if len(p.entries[i].key) != len(p.entries[j].key) {
			return len(p.entries[i].key) > len(p.entries[j].key)
		}
		return p.entries[i].key < p.entries[j].key
	})
	return p
}

// Lookup returns the price for model and whether one matched.
func (p *Pricer) Lookup(model string) (Price, bool) {
	if p == nil {
		return Price{}, false
	}
	// Treat dotted versions (claude-opus-5.5) like dashed ones (claude-opus-5-5).
	model = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(model)), ".", "-")
	for _, entry := range p.entries {
		if strings.Contains(model, entry.key) {
			return entry.price, true
		}
	}
	return Price{}, false
}

// LoadOverrides reads a model-pricing.json file; a missing file yields nil.
func LoadOverrides(path string) (map[string]Price, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out map[string]Price
	if err = json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
