package middleware

import (
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
)

func TestClickHouseLoggingCapturesRequestAndRedactsCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var events []*clickhouselog.Event
	sink := clickhouselog.NewWithInserter(func(_ context.Context, batch []*clickhouselog.Event) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, batch...)
		return nil
	})

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
	if len(events) != 1 {
		t.Fatalf("captured %d events, want 1 (management paths are skipped)", len(events))
	}
	event := events[0]
	requestHeaders := clickhouselog.HeadersJSON(event.RequestHeaders)
	if strings.Contains(requestHeaders, "super-secret") || strings.Contains(requestHeaders, "sk-secret") {
		t.Fatalf("credentials leaked: %s", requestHeaders)
	}
	if got := string(event.RequestBody); got != body {
		t.Fatalf("request body = %q", got)
	}
	if !strings.Contains(event.UserAgent, "claude-desktop") {
		t.Fatalf("user agent = %q", event.UserAgent)
	}
	if event.Status != 200 {
		t.Fatalf("status = %d", event.Status)
	}
	if got := string(event.ResponseBody); got != "ok-response-body" {
		t.Fatalf("response body = %q", got)
	}
	responseHeaders := clickhouselog.HeadersJSON(event.ResponseHeaders)
	if got := ojson.Get(responseHeaders, "X-Reply.0").String(); got != "hello" {
		t.Fatalf("response X-Reply = %q", got)
	}
	if strings.Contains(responseHeaders, "secret-cookie") {
		t.Fatalf("response cookie leaked: %s", responseHeaders)
	}
}

func TestClickHouseLoggingCapturesBodyOfRejectedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var events []*clickhouselog.Event
	sink := clickhouselog.NewWithInserter(func(_ context.Context, batch []*clickhouselog.Event) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, batch...)
		return nil
	})

	router := gin.New()
	router.Use(ClickHouseLoggingMiddleware(sink))
	// Rejects before ever reading the body, like an authentication failure.
	router.POST("/v1/messages", func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) })

	body := `{"model":"claude-opus-4-6"}`
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	sink.Close(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("captured %d events, want 1", len(events))
	}
	if got := string(events[0].RequestBody); got != body {
		t.Fatalf("request body = %q, want %q", got, body)
	}
	if events[0].Status != http.StatusUnauthorized {
		t.Fatalf("status = %d", events[0].Status)
	}
}
