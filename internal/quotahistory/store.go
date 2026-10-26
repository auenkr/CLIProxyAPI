// Package quotahistory keeps a bounded, file-backed time series of credential quota samples.
package quotahistory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// Retention is how long samples are kept.
	Retention = 14 * 24 * time.Hour
	// minInterval drops samples of the same credential that arrive too close together.
	minInterval = time.Minute
	maxBatch    = 500
)

// Sample is one observation of a credential's remaining quota percent (0..100).
type Sample struct {
	Provider  string  `json:"provider"`
	Name      string  `json:"name"`
	At        int64   `json:"at"` // Unix milliseconds
	Remaining float64 `json:"remaining"`
}

// Store is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	path    string
	samples []Sample
	last    map[string]int64
	now     func() time.Time
}

// New loads the store from path (missing file is fine). An empty path keeps history in memory only.
func New(path string) *Store {
	s := &Store{path: path, last: map[string]int64{}, now: time.Now}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("quota history: read %s: %v", path, err)
		}
		return s
	}
	if err = json.Unmarshal(data, &s.samples); err != nil {
		log.Warnf("quota history: parse %s: %v", path, err)
		s.samples = nil
		return s
	}
	for _, sample := range s.samples {
		key := sample.Provider + "\x00" + sample.Name
		if sample.At > s.last[key] {
			s.last[key] = sample.At
		}
	}
	return s
}

// Add records valid samples and returns how many were kept.
func (s *Store) Add(batch []Sample) int {
	if len(batch) > maxBatch {
		batch = batch[:maxBatch]
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	nowMs := s.now().UnixMilli()
	kept := 0
	for _, sample := range batch {
		if sample.Name == "" || sample.Remaining < 0 || sample.Remaining > 100 {
			continue
		}
		if sample.At <= 0 || sample.At > nowMs+int64(time.Minute/time.Millisecond) {
			sample.At = nowMs
		}
		if sample.At < nowMs-int64(Retention/time.Millisecond) {
			continue
		}
		key := sample.Provider + "\x00" + sample.Name
		if sample.At-s.last[key] < int64(minInterval/time.Millisecond) && sample.At >= s.last[key] {
			continue
		}
		s.last[key] = sample.At
		s.samples = append(s.samples, sample)
		kept++
	}
	if kept == 0 {
		return 0
	}
	s.pruneLocked(nowMs)
	s.saveLocked()
	return kept
}

// Since returns samples at or after sinceMs ordered by time.
func (s *Store) Since(sinceMs int64) []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Sample, 0, len(s.samples))
	for _, sample := range s.samples {
		if sample.At >= sinceMs {
			out = append(out, sample)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

func (s *Store) pruneLocked(nowMs int64) {
	cutoff := nowMs - int64(Retention/time.Millisecond)
	kept := s.samples[:0]
	for _, sample := range s.samples {
		if sample.At >= cutoff {
			kept = append(kept, sample)
		}
	}
	s.samples = kept
}

func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.samples)
	if err != nil {
		log.Warnf("quota history: encode: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err = os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		log.Warnf("quota history: mkdir: %v", err)
		return
	}
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		log.Warnf("quota history: write: %v", err)
		return
	}
	if err = os.Rename(tmp, s.path); err != nil {
		log.Warnf("quota history: rename: %v", err)
	}
}
