package clickhouselog

import (
	"bufio"
	"compress/gzip"
	"context"
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

func TestEventEncodeKeepsBodyAndEscapes(t *testing.T) {
	event := &Event{
		Method:         "POST",
		Path:           "/v1/messages",
		Status:         200,
		RequestHeaders: http.Header{"X-Test": {"a\"b"}},
		RequestBody:    []byte("{\"model\":\"x\",\n\"bad\":\"\xff\"}"),
	}
	line := event.Encode()
	if strings.ContainsRune(string(line), '\n') {
		t.Fatalf("NDJSON line contains a raw newline: %q", line)
	}
	if got := ojson.GetBytes(line, "status").Int(); got != 200 {
		t.Fatalf("status = %d", got)
	}
	if body := ojson.GetBytes(line, "request_body").String(); !strings.Contains(body, "\"model\":\"x\"") {
		t.Fatalf("body not preserved: %q", body)
	}
}

func TestSinkDeliversBatchAsGzippedNDJSON(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	var user, key, query string
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
		user = r.Header.Get("X-ClickHouse-User")
		key = r.Header.Get("X-ClickHouse-Key")
		query = r.URL.RawQuery
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	sink := newSink(server.URL, "ch-user", "tok", "proxy_requests", server.Client())
	for i := 0; i < 3; i++ {
		sink.Enqueue((&Event{Method: "GET", Path: "/x", Status: 200}).Encode())
	}
	sink.Close(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 3 {
		t.Fatalf("delivered %d lines, want 3", len(lines))
	}
	if user != "ch-user" || key != "tok" {
		t.Fatalf("auth headers = %q / %q", user, key)
	}
	if !strings.Contains(query, "INSERT+INTO+proxy_requests+FORMAT+JSONEachRow") {
		t.Fatalf("query = %q", query)
	}
}

func TestNewIsDisabledUnlessEnabledWithToken(t *testing.T) {
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

func TestBaseURLSchemeSelection(t *testing.T) {
	cases := map[string]string{
		baseURL("ch.example.test", 443):           "https://ch.example.test:443",
		baseURL("ch.example.test", 8123):          "http://ch.example.test:8123",
		baseURL("http://ch.example.test:9000", 1): "http://ch.example.test:9000",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("baseURL = %q, want %q", got, want)
		}
	}
}

func TestNilSinkIsSafe(t *testing.T) {
	var sink *Sink
	sink.Enqueue([]byte("{}"))
	sink.Close(context.Background())
}
