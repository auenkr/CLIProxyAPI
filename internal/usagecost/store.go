// Package usagecost tracks API-equivalent token cost per hour, provider and model.
package usagecost

import (
	"context"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/statemirror"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	// Retention bounds how long hourly buckets are kept.
	Retention     = 35 * 24 * time.Hour
	flushInterval = 30 * time.Second
	hourMs        = int64(time.Hour / time.Millisecond)
)

// Tokens is the non-overlapping token split used for pricing.
type Tokens struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
}

func (t *Tokens) add(o Tokens) {
	t.Input += o.Input
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.Output += o.Output
	t.Reasoning += o.Reasoning
}

// Total is the sum of every token class.
func (t Tokens) Total() int64 {
	return t.Input + t.CacheRead + t.CacheWrite + t.Output + t.Reasoning
}

type bucket struct {
	Hour     int64  `json:"hour"` // Unix milliseconds, truncated to the hour
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   Tokens `json:"tokens"`
}

// Costs is a USD cost split by token class.
type Costs struct {
	Input      float64 `json:"input"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Output     float64 `json:"output"`
	Total      float64 `json:"total"`
}

func (c *Costs) add(o Costs) {
	c.Input += o.Input
	c.CacheRead += o.CacheRead
	c.CacheWrite += o.CacheWrite
	c.Output += o.Output
	c.Total += o.Total
}

// Store is safe for concurrent use and implements coreusage.Plugin.
type Store struct {
	mu       sync.Mutex
	path     string
	buckets  map[string]*bucket
	now      func() time.Time
	revision atomic.Uint64
	saved    uint64
	pricer   atomic.Pointer[Pricer]

	mirror    statemirror.Mirror
	mirrorKey string
}

// New loads the store from path (a missing file is fine). An empty path is memory only.
func New(path string, pricer *Pricer) *Store {
	s := &Store{path: path, buckets: map[string]*bucket{}, now: time.Now}
	s.pricer.Store(pricer)
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("usage cost: read %s: %v", path, err)
		}
		return s
	}
	var list []bucket
	if err = json.Unmarshal(data, &list); err != nil {
		log.Warnf("usage cost: parse %s: %v", path, err)
		return s
	}
	for i := range list {
		b := list[i]
		s.buckets[key(b.Hour, b.Provider, b.Model)] = &b
	}
	s.prune()
	return s
}

// SetMirror mirrors flushed state to m under key.
func (s *Store) SetMirror(m statemirror.Mirror, key string) {
	s.mu.Lock()
	s.mirror, s.mirrorKey = m, key
	s.mu.Unlock()
}

// SetPricer swaps the price table; costs are computed at query time.
func (s *Store) SetPricer(p *Pricer) { s.pricer.Store(p) }

func key(hour int64, provider, model string) string {
	return provider + "\x00" + model + "\x00" + time.UnixMilli(hour).UTC().Format(time.RFC3339)
}

// HandleUsage implements coreusage.Plugin.
func (s *Store) HandleUsage(_ context.Context, record coreusage.Record) {
	if s == nil || record.Failed {
		return
	}
	detail := record.Detail
	bd := detail.TokenBreakdown
	if bd.SchemaVersion != coreusage.TokenAccountingSchemaVersion {
		bd = coreusage.NewSubsetTokenBreakdown(detail.InputTokens, detail.CacheReadTokens,
			detail.CacheCreationTokens, detail.OutputTokens, detail.ReasoningTokens, detail.TotalTokens)
	}
	tokens := Tokens{
		Input:      bd.Input.UncachedTokens,
		CacheRead:  bd.Input.CacheReadTokens,
		CacheWrite: bd.Input.CacheWriteTokens,
		Output:     bd.Output.NonReasoningTokens,
		Reasoning:  bd.Output.ReasoningTokens,
	}
	if tokens.Total() <= 0 {
		return
	}
	model := strings.TrimSpace(record.Model)
	if model == "" {
		model = "unknown"
	}
	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	if provider == "" {
		provider = "unknown"
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = s.now()
	}
	hour := at.UnixMilli() / hourMs * hourMs

	s.mu.Lock()
	k := key(hour, provider, model)
	b := s.buckets[k]
	if b == nil {
		b = &bucket{Hour: hour, Provider: provider, Model: model}
		s.buckets[k] = b
	}
	b.Requests++
	b.Tokens.add(tokens)
	s.mu.Unlock()
	s.revision.Add(1)
}

func (s *Store) prune() {
	cutoff := s.now().Add(-Retention).UnixMilli()
	for k, b := range s.buckets {
		if b.Hour < cutoff {
			delete(s.buckets, k)
		}
	}
}

// Run flushes dirty state periodically until ctx ends, then flushes once more.
func (s *Store) Run(ctx context.Context) {
	if s == nil || s.path == "" {
		return
	}
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flush()
			return
		case <-ticker.C:
			s.flush()
		}
	}
}

func (s *Store) flush() {
	s.mu.Lock()
	rev := s.revision.Load()
	if rev == s.saved {
		s.mu.Unlock()
		return
	}
	s.prune()
	mirror, mirrorKey := s.mirror, s.mirrorKey
	list := make([]bucket, 0, len(s.buckets))
	for _, b := range s.buckets {
		list = append(list, *b)
	}
	s.mu.Unlock()

	data, err := json.Marshal(list)
	if err != nil {
		log.Warnf("usage cost: marshal: %v", err)
		return
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		log.Warnf("usage cost: mkdir: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		log.Warnf("usage cost: write: %v", err)
		return
	}
	if err = os.Rename(tmp, s.path); err != nil {
		log.Warnf("usage cost: rename: %v", err)
		return
	}
	if mirror != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		errMirror := mirror.SaveState(ctx, mirrorKey, data)
		cancel()
		if errMirror != nil {
			log.Warnf("usage cost: mirror: %v", errMirror)
			return
		}
	}
	s.mu.Lock()
	s.saved = rev
	s.mu.Unlock()
}

// HourPoint is the cost of one hour split by provider.
type HourPoint struct {
	Hour       int64              `json:"hour"`
	Total      float64            `json:"total"`
	ByProvider map[string]float64 `json:"by_provider"`
}

// ModelRow is one row of the model breakdown.
type ModelRow struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Requests int64   `json:"requests"`
	Tokens   Tokens  `json:"tokens"`
	Costs    Costs   `json:"costs"`
	Priced   bool    `json:"priced"`
	Share    float64 `json:"share"`
}

// ProviderRow is one row of the per-provider summary.
type ProviderRow struct {
	Provider string `json:"provider"`
	Requests int64  `json:"requests"`
	Tokens   Tokens `json:"tokens"`
	Costs    Costs  `json:"costs"`
}

// Report is the query result.
type Report struct {
	Since        int64         `json:"since"`
	Until        int64         `json:"until"`
	Requests     int64         `json:"requests"`
	Tokens       Tokens        `json:"tokens"`
	Costs        Costs         `json:"costs"`
	CacheSavings float64       `json:"cache_savings"`
	Unpriced     int64         `json:"unpriced_tokens"`
	Hourly       []HourPoint   `json:"hourly"`
	Models       []ModelRow    `json:"models"`
	Providers    []ProviderRow `json:"providers"`
}

func priceTokens(t Tokens, p Price) Costs {
	const per = 1e6
	c := Costs{
		Input:      float64(t.Input) * p.Input / per,
		CacheRead:  float64(t.CacheRead) * p.CacheRead / per,
		CacheWrite: float64(t.CacheWrite) * p.CacheWrite / per,
		// Reasoning tokens are billed at the output rate.
		Output: float64(t.Output+t.Reasoning) * p.Output / per,
	}
	c.Total = c.Input + c.CacheRead + c.CacheWrite + c.Output
	return c
}

// Query aggregates buckets within [since, now].
func (s *Store) Query(since time.Time) Report {
	pricer := s.pricer.Load()
	now := s.now()
	sinceMs := since.UnixMilli()
	report := Report{Since: sinceMs, Until: now.UnixMilli()}

	s.mu.Lock()
	list := make([]bucket, 0, len(s.buckets))
	for _, b := range s.buckets {
		if b.Hour >= sinceMs {
			list = append(list, *b)
		}
	}
	s.mu.Unlock()

	hours := map[int64]*HourPoint{}
	models := map[string]*ModelRow{}
	provs := map[string]*ProviderRow{}
	for _, b := range list {
		price, priced := pricer.Lookup(b.Model)
		costs := priceTokens(b.Tokens, price)

		report.Requests += b.Requests
		report.Tokens.add(b.Tokens)
		report.Costs.add(costs)
		if priced {
			report.CacheSavings += float64(b.Tokens.CacheRead) * (price.Input - price.CacheRead) / 1e6
		} else {
			report.Unpriced += b.Tokens.Total()
		}

		hp := hours[b.Hour]
		if hp == nil {
			hp = &HourPoint{Hour: b.Hour, ByProvider: map[string]float64{}}
			hours[b.Hour] = hp
		}
		hp.Total += costs.Total
		hp.ByProvider[b.Provider] += costs.Total

		mk := b.Provider + "\x00" + b.Model
		mr := models[mk]
		if mr == nil {
			mr = &ModelRow{Provider: b.Provider, Model: b.Model, Priced: priced}
			models[mk] = mr
		}
		mr.Requests += b.Requests
		mr.Tokens.add(b.Tokens)
		mr.Costs.add(costs)

		pr := provs[b.Provider]
		if pr == nil {
			pr = &ProviderRow{Provider: b.Provider}
			provs[b.Provider] = pr
		}
		pr.Requests += b.Requests
		pr.Tokens.add(b.Tokens)
		pr.Costs.add(costs)
	}

	report.Hourly = make([]HourPoint, 0, len(hours))
	for _, hp := range hours {
		report.Hourly = append(report.Hourly, *hp)
	}
	sort.Slice(report.Hourly, func(i, j int) bool { return report.Hourly[i].Hour < report.Hourly[j].Hour })

	report.Models = make([]ModelRow, 0, len(models))
	for _, mr := range models {
		if report.Costs.Total > 0 {
			mr.Share = mr.Costs.Total / report.Costs.Total
		}
		report.Models = append(report.Models, *mr)
	}
	sort.Slice(report.Models, func(i, j int) bool {
		if report.Models[i].Costs.Total != report.Models[j].Costs.Total {
			return report.Models[i].Costs.Total > report.Models[j].Costs.Total
		}
		return report.Models[i].Model < report.Models[j].Model
	})

	report.Providers = make([]ProviderRow, 0, len(provs))
	for _, pr := range provs {
		report.Providers = append(report.Providers, *pr)
	}
	sort.Slice(report.Providers, func(i, j int) bool {
		if report.Providers[i].Costs.Total != report.Providers[j].Costs.Total {
			return report.Providers[i].Costs.Total > report.Providers[j].Costs.Total
		}
		return report.Providers[i].Provider < report.Providers[j].Provider
	})
	return report
}
