// Package tinybirdlog ships captured proxy requests to Tinybird (ClickHouse) asynchronously.
// Request handling never waits on Tinybird: events are queued in memory and a background
// worker posts them in NDJSON batches through the Events API.
package tinybirdlog

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	defaultHost       = "https://api.ap-east-1.aws.tinybird.co"
	defaultDatasource = "proxy_requests"

	queueSize         = 1024
	maxQueuedBytes    = 64 << 20 // events are dropped once this many bytes are waiting
	maxBatchEvents    = 200
	maxBatchBytes     = 5 << 20
	flushInterval     = time.Second
	maxSendAttempts   = 4
	retryBackoffStart = 500 * time.Millisecond
)

// Sink queues events and delivers them to Tinybird in the background.
type Sink struct {
	endpoint string
	token    string
	client   *http.Client

	queue       chan []byte
	queuedBytes atomic.Int64
	dropped     atomic.Uint64

	pending   sync.WaitGroup
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// New builds a started Sink from configuration. It returns nil when capture is
// disabled or no token is set.
func New(cfg config.TinybirdConfig) *Sink {
	token := strings.TrimSpace(cfg.Token)
	if !cfg.Enable || token == "" {
		return nil
	}
	host := strings.TrimRight(strings.TrimSpace(cfg.Host), "/")
	if host == "" {
		host = defaultHost
	}
	datasource := strings.TrimSpace(cfg.Datasource)
	if datasource == "" {
		datasource = defaultDatasource
	}
	return newSink(host, token, datasource, &http.Client{})
}

func newSink(host, token, datasource string, client *http.Client) *Sink {
	query := url.Values{"name": {datasource}, "wait": {"false"}}
	s := &Sink{
		endpoint: host + "/v0/events?" + query.Encode(),
		token:    token,
		client:   client,
		queue:    make(chan []byte, queueSize),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go s.run()
	log.Infof("tinybird request logging enabled: host=%s datasource=%s", host, datasource)
	return s
}

// Enqueue adds one encoded NDJSON line. It never blocks; the event is dropped when the queue is full.
func (s *Sink) Enqueue(line []byte) {
	if s == nil || len(line) == 0 {
		return
	}
	size := int64(len(line))
	if s.queuedBytes.Add(size) > maxQueuedBytes {
		s.queuedBytes.Add(-size)
		s.noteDrop()
		return
	}
	select {
	case s.queue <- line:
	default:
		s.queuedBytes.Add(-size)
		s.noteDrop()
	}
}

func (s *Sink) noteDrop() {
	if n := s.dropped.Add(1); n == 1 || n%100 == 0 {
		log.Warnf("tinybird request logging: queue full, %d events dropped so far", n)
	}
}

// Async runs fn in its own goroutine and makes Close wait for it, so an event that is
// still being finished at shutdown is not lost.
func (s *Sink) Async(fn func()) {
	if s == nil {
		return
	}
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		fn()
	}()
}

// Close flushes queued events and stops the worker, giving up when ctx ends.
func (s *Sink) Close(ctx context.Context) {
	if s == nil {
		return
	}
	pendingDone := make(chan struct{})
	go func() {
		s.pending.Wait()
		close(pendingDone)
	}()
	select {
	case <-pendingDone:
	case <-ctx.Done():
		log.Warn("tinybird request logging: in-flight events abandoned at shutdown")
	}
	s.closeOnce.Do(func() { close(s.stop) })
	select {
	case <-s.done:
	case <-ctx.Done():
		log.Warn("tinybird request logging: shutdown flush abandoned")
	}
}

func (s *Sink) run() {
	defer close(s.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	var batch [][]byte
	batchBytes := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.send(batch)
		batch = batch[:0]
		batchBytes = 0
	}
	add := func(line []byte) {
		s.queuedBytes.Add(-int64(len(line)))
		batch = append(batch, line)
		batchBytes += len(line) + 1
		if len(batch) >= maxBatchEvents || batchBytes >= maxBatchBytes {
			flush()
		}
	}

	for {
		select {
		case line := <-s.queue:
			add(line)
		case <-ticker.C:
			flush()
		case <-s.stop:
			for {
				select {
				case line := <-s.queue:
					add(line)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *Sink) send(batch [][]byte) {
	var raw bytes.Buffer
	for _, line := range batch {
		raw.Write(line)
		raw.WriteByte('\n')
	}
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		log.WithError(err).Warn("tinybird request logging: compress batch")
		return
	}
	if err := gz.Close(); err != nil {
		log.WithError(err).Warn("tinybird request logging: compress batch")
		return
	}

	backoff := retryBackoffStart
	var lastErr error
	for attempt := 1; attempt <= maxSendAttempts; attempt++ {
		retry, err := s.post(body.Bytes())
		if err == nil {
			return
		}
		lastErr = err
		if !retry || attempt == maxSendAttempts {
			break
		}
		select {
		case <-time.After(backoff):
			backoff *= 2
		case <-s.stop:
			// Shutting down: stop retrying instead of delaying exit.
			attempt = maxSendAttempts - 1
		}
	}
	log.WithError(lastErr).Warnf("tinybird request logging: dropped batch of %d events", len(batch))
}

// post sends one request and reports whether a failure is worth retrying.
func (s *Sink) post(body []byte) (retry bool, err error) {
	req, err := http.NewRequest(http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := s.client.Do(req)
	if err != nil {
		return true, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("tinybird request logging: close response body")
		}
	}()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false, nil
	}
	retry = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	return retry, fmt.Errorf("tinybird returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}
