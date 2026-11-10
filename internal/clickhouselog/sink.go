// Package clickhouselog ships captured proxy requests to ClickHouse asynchronously.
// Request handling never waits on ClickHouse: events are queued in memory and a background
// worker inserts them in batches with the official ClickHouse Go client.
package clickhouselog

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	defaultPort     = 443
	defaultUsername = "default"
	defaultTable    = "proxy_requests"

	queueSize         = 1024
	maxQueuedBytes    = 64 << 20 // events are dropped once this many bytes are waiting
	maxBatchEvents    = 200
	maxBatchBytes     = 5 << 20
	flushInterval     = time.Second
	maxSendAttempts   = 4
	retryBackoffStart = 500 * time.Millisecond
)

// Inserter writes one batch of events to ClickHouse.
type Inserter func(ctx context.Context, batch []*Event) error

// Sink queues events and delivers them to ClickHouse in the background.
type Sink struct {
	insert Inserter

	queue       chan *Event
	queuedBytes atomic.Int64
	dropped     atomic.Uint64

	pending   sync.WaitGroup
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// New builds a started Sink from configuration. It returns nil when capture is
// disabled, host or token is missing, or the client cannot be created.
func New(cfg config.ClickHouseConfig) *Sink {
	if ingestURL := strings.TrimSpace(cfg.IngestURL); cfg.Enable && ingestURL != "" {
		token := strings.TrimSpace(cfg.IngestToken)
		if token == "" {
			return nil
		}
		log.Infof("clickhouse request logging enabled: http ingest endpoint=%s", ingestURL)
		return NewWithInserter(httpInserter(ingestURL, token, &http.Client{}))
	}
	token := strings.TrimSpace(cfg.Token)
	host := strings.TrimSpace(cfg.Host)
	if !cfg.Enable || token == "" || host == "" {
		return nil
	}
	port := cfg.Port
	if port <= 0 {
		port = defaultPort
	}
	username := strings.TrimSpace(cfg.Username)
	if username == "" {
		username = defaultUsername
	}
	table := strings.TrimSpace(cfg.Table)
	if table == "" {
		table = defaultTable
	}
	if database := strings.TrimSpace(cfg.Database); database != "" {
		table = database + "." + table
	}

	secure := port != 8123 && port != 9000
	if rest, ok := strings.CutPrefix(host, "http://"); ok {
		host, secure = rest, false
	} else if rest, ok := strings.CutPrefix(host, "https://"); ok {
		host, secure = rest, true
	}
	host = strings.TrimRight(host, "/")

	options := &clickhouse.Options{
		Addr:     []string{fmt.Sprintf("%s:%d", host, port)},
		Protocol: clickhouse.HTTP,
		Auth: clickhouse.Auth{
			Database: strings.TrimSpace(cfg.Database),
			Username: username,
			Password: token,
		},
	}
	if secure {
		options.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	conn, err := clickhouse.Open(options)
	if err != nil {
		log.WithError(err).Warn("clickhouse request logging: open client")
		return nil
	}
	log.Infof("clickhouse request logging enabled: addr=%s table=%s user=%s", options.Addr[0], table, username)
	return NewWithInserter(sdkInserter(conn, table))
}

// NewWithInserter builds a started Sink that delivers batches through insert.
func NewWithInserter(insert Inserter) *Sink {
	s := &Sink{
		insert: insert,
		queue:  make(chan *Event, queueSize),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go s.run()
	return s
}

// httpInserter posts batches as gzipped NDJSON with a bearer token.
func httpInserter(endpoint, token string, client *http.Client) Inserter {
	return func(ctx context.Context, events []*Event) error {
		var body bytes.Buffer
		gz := gzip.NewWriter(&body)
		for _, e := range events {
			if _, err := gz.Write(append(e.Encode(), '\n')); err != nil {
				return fmt.Errorf("compress batch: %w", err)
			}
		}
		if err := gz.Close(); err != nil {
			return fmt.Errorf("compress batch: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-ndjson")
		req.Header.Set("Content-Encoding", "gzip")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.WithError(errClose).Debug("clickhouse request logging: close response body")
			}
		}()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("ingest returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		}
		return nil
	}
}

// sdkInserter inserts batches with the official client's batch API.
func sdkInserter(conn clickhouse.Conn, table string) Inserter {
	query := "INSERT INTO " + table + " (" + strings.Join(columns, ", ") + ")"
	return func(ctx context.Context, events []*Event) error {
		batch, err := conn.PrepareBatch(ctx, query)
		if err != nil {
			return fmt.Errorf("prepare batch: %w", err)
		}
		for _, e := range events {
			if err = batch.Append(e.row()...); err != nil {
				_ = batch.Abort()
				return fmt.Errorf("append row: %w", err)
			}
		}
		if err = batch.Send(); err != nil {
			return fmt.Errorf("send batch: %w", err)
		}
		return nil
	}
}

// Enqueue adds one event. It never blocks; the event is dropped when the queue is full.
func (s *Sink) Enqueue(event *Event) {
	if s == nil || event == nil {
		return
	}
	size := event.size()
	if s.queuedBytes.Add(size) > maxQueuedBytes {
		s.queuedBytes.Add(-size)
		s.noteDrop()
		return
	}
	select {
	case s.queue <- event:
	default:
		s.queuedBytes.Add(-size)
		s.noteDrop()
	}
}

func (s *Sink) noteDrop() {
	if n := s.dropped.Add(1); n == 1 || n%100 == 0 {
		log.Warnf("clickhouse request logging: queue full, %d events dropped so far", n)
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
		log.Warn("clickhouse request logging: in-flight events abandoned at shutdown")
	}
	s.closeOnce.Do(func() { close(s.stop) })
	select {
	case <-s.done:
	case <-ctx.Done():
		log.Warn("clickhouse request logging: shutdown flush abandoned")
	}
}

func (s *Sink) run() {
	defer close(s.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	var batch []*Event
	batchBytes := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.send(batch)
		batch = batch[:0]
		batchBytes = 0
	}
	add := func(event *Event) {
		size := event.size()
		s.queuedBytes.Add(-size)
		batch = append(batch, event)
		batchBytes += int(size)
		if len(batch) >= maxBatchEvents || batchBytes >= maxBatchBytes {
			flush()
		}
	}

	for {
		select {
		case event := <-s.queue:
			add(event)
		case <-ticker.C:
			flush()
		case <-s.stop:
			for {
				select {
				case event := <-s.queue:
					add(event)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *Sink) send(batch []*Event) {
	backoff := retryBackoffStart
	var lastErr error
	for attempt := 1; attempt <= maxSendAttempts; attempt++ {
		lastErr = s.insert(context.Background(), batch)
		if lastErr == nil {
			return
		}
		if attempt == maxSendAttempts {
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
	log.WithError(lastErr).Warnf("clickhouse request logging: dropped batch of %d events", len(batch))
}
