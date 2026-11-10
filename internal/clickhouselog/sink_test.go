package clickhouselog

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestHeadersJSONRedactsCredentials(t *testing.T) {
	headers := http.Header{
		"Authorization":  {"Bearer secret-token"},
		"X-Api-Key":      {"sk-secret"},
		"X-Goog-Api-Key": {"goog-secret"},
		"Cookie":         {"session=secret"},
		"User-Agent":     {"claude-cli/2.1.0 (external, cli)"},
		"Anthropic-Beta": {"claude-code-20250219", "oauth-2025-04-20"},
	}
	out := HeadersJSON(headers)
	for _, secret := range []string{"secret-token", "sk-secret", "goog-secret", "session=secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("header output leaks %q: %s", secret, out)
		}
	}
	if got := ojson.Get(out, "User-Agent.0").String(); got != "claude-cli/2.1.0 (external, cli)" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := ojson.Get(out, "Anthropic-Beta.1").String(); got != "oauth-2025-04-20" {
		t.Fatalf("Anthropic-Beta[1] = %q", got)
	}
	if got := ojson.Get(out, "Authorization.0").String(); got != Redacted {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestEventRowMatchesColumnsAndTypes(t *testing.T) {
	event := &Event{
		Method:          "POST",
		Path:            "/v1/messages",
		Status:          200,
		DurationMs:      -5,
		RequestHeaders:  http.Header{"X-Test": {"a\"b"}},
		RequestBody:     []byte("{\"bad\":\"\xff\"}"),
		RequestBodySize: 1 << 40,
	}
	row := event.row()
	if len(row) != len(columns) {
		t.Fatalf("row has %d values for %d columns", len(row), len(columns))
	}
	if got, ok := row[5].(uint16); !ok || got != 200 {
		t.Fatalf("status = %#v", row[5])
	}
	if got, ok := row[6].(uint32); !ok || got != 0 {
		t.Fatalf("negative duration should clamp to 0, got %#v", row[6])
	}
	if got, ok := row[11].(uint32); !ok || got != ^uint32(0) {
		t.Fatalf("oversized body size should clamp, got %#v", row[11])
	}
	if body, _ := row[10].(string); strings.ContainsRune(body, 0xff) || !strings.Contains(body, "bad") {
		t.Fatalf("body not sanitised: %q", body)
	}
}

func TestSinkDeliversEventsInBatches(t *testing.T) {
	var mu sync.Mutex
	var got []*Event
	sink := NewWithInserter(func(_ context.Context, batch []*Event) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, batch...)
		return nil
	})
	for i := 0; i < 3; i++ {
		sink.Enqueue(&Event{Method: "GET", Path: "/x", Status: 200})
	}
	sink.Close(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("delivered %d events, want 3", len(got))
	}
}

func TestSinkRetriesThenDrops(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	sink := NewWithInserter(func(context.Context, []*Event) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		return errors.New("boom")
	})
	sink.Enqueue(&Event{Path: "/x"})
	sink.Close(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if attempts < 1 {
		t.Fatal("insert was never attempted")
	}
}

func TestNewIsDisabledUnlessConfigured(t *testing.T) {
	if New(config.ClickHouseConfig{Host: "ch.example.test", Token: "tok"}) != nil {
		t.Fatal("sink created although enable is false")
	}
	if New(config.ClickHouseConfig{Enable: true, Host: "ch.example.test"}) != nil {
		t.Fatal("sink created without a token")
	}
	if New(config.ClickHouseConfig{Enable: true, Token: "tok"}) != nil {
		t.Fatal("sink created without a host")
	}
}

func TestNilSinkIsSafe(t *testing.T) {
	var sink *Sink
	sink.Enqueue(&Event{})
	sink.Close(context.Background())
}

func TestHTTPIngestPostsGzippedNDJSON(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip: %v", err)
			return
		}
		scanner := bufio.NewScanner(zr)
		scanner.Buffer(make([]byte, 1<<20), 1<<24)
		mu.Lock()
		defer mu.Unlock()
		auth = r.Header.Get("Authorization")
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	sink := New(config.ClickHouseConfig{Enable: true, IngestURL: server.URL, IngestToken: "tok"})
	if sink == nil {
		t.Fatal("sink not created for ingest url")
	}
	sink.Enqueue(&Event{Method: "POST", Path: "/v1/messages", Status: 200, RequestBody: []byte(`{"a":"b"}`)})
	sink.Close(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if auth != "Bearer tok" {
		t.Fatalf("Authorization = %q", auth)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if got := ojson.Get(lines[0], "request_body").String(); got != `{"a":"b"}` {
		t.Fatalf("request_body = %q", got)
	}
	if got := ojson.Get(lines[0], "status").Int(); got != 200 {
		t.Fatalf("status = %d", got)
	}
	if got := ojson.Get(lines[0], "timestamp").String(); got == "" {
		t.Fatal("timestamp missing")
	}
}

func TestHTTPIngestNeedsToken(t *testing.T) {
	if New(config.ClickHouseConfig{Enable: true, IngestURL: "https://example.test"}) != nil {
		t.Fatal("sink created without ingest token")
	}
}
