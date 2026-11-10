package middleware

import (
	"bufio"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouselog"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestClickHouseLoggingCapturesRequestAndRedactsCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var lines []string
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip: %v", err)
			return
		}
		scanner := bufio.NewScanner(zr)
		scanner.Buffer(make([]byte, 1<<20), 1<<24)
		mu.Lock()
		defer mu.Unlock()
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ingest.Close()

	sink := clickhouselog.New(config.ClickHouseConfig{Enable: true, Username: "u", Token: "tok", Host: ingest.URL})

	var handlerSaw string
	router := gin.New()
	router.Use(ClickHouseLoggingMiddleware(sink))
	router.POST("/v1/messages", func(c *gin.Context) {
		data, _ := io.ReadAll(c.Request.Body)
		handlerSaw = string(data)
		c.Header("X-Reply", "hello")
		c.Header("Set-Cookie", "sid=secret-cookie")
		c.String(http.StatusOK, "ok-response-body")
	})
	router.POST("/v0/management/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	body := `{"model":"claude-haiku-5-5"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages?key=abc", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer super-secret")
	req.Header.Set("X-Api-Key", "sk-secret")
	req.Header.Set("User-Agent", "claude-cli/2.1.0 (external, claude-desktop)")
	router.ServeHTTP(httptest.NewRecorder(), req)
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v0/management/x", strings.NewReader("{}")))

	sink.Close(context.Background())

	if handlerSaw != body {
		t.Fatalf("handler saw %q, want the untouched body", handlerSaw)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 {
		t.Fatalf("captured %d events, want 1 (management paths are skipped): %v", len(lines), lines)
	}
	line := lines[0]
	if strings.Contains(line, "super-secret") || strings.Contains(line, "sk-secret") {
		t.Fatalf("credentials leaked: %s", line)
	}
	if got := ojson.Get(line, "request_body").String(); got != body {
		t.Fatalf("request_body = %q", got)
	}
	if got := ojson.Get(line, "user_agent").String(); !strings.Contains(got, "claude-desktop") {
		t.Fatalf("user_agent = %q", got)
	}
	if got := ojson.Get(line, "status").Int(); got != 200 {
		t.Fatalf("status = %d", got)
	}
	if got := ojson.Get(line, "response_body").String(); got != "ok-response-body" {
		t.Fatalf("response_body = %q", got)
	}
	if got := ojson.Get(ojson.Get(line, "response_headers").String(), "X-Reply.0").String(); got != "hello" {
		t.Fatalf("response X-Reply = %q", got)
	}
	if strings.Contains(line, "secret-cookie") {
		t.Fatalf("response cookie leaked: %s", line)
	}
}

func TestClickHouseLoggingCapturesBodyOfRejectedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var lines []string
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip: %v", err)
			return
		}
		scanner := bufio.NewScanner(zr)
		mu.Lock()
		defer mu.Unlock()
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ingest.Close()

	sink := clickhouselog.New(config.ClickHouseConfig{Enable: true, Username: "u", Token: "tok", Host: ingest.URL})

	router := gin.New()
	router.Use(ClickHouseLoggingMiddleware(sink))
	// Rejects before ever reading the body, like an authentication failure.
	router.POST("/v1/messages", func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) })

	body := `{"model":"claude-opus-4-6"}`
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	sink.Close(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 {
		t.Fatalf("captured %d events, want 1", len(lines))
	}
	if got := ojson.Get(lines[0], "request_body").String(); got != body {
		t.Fatalf("request_body = %q, want %q", got, body)
	}
	if got := ojson.Get(lines[0], "status").Int(); got != http.StatusUnauthorized {
		t.Fatalf("status = %d", got)
	}
}
