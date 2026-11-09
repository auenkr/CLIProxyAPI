package middleware

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/tinybirdlog"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const (
	// tinybirdMaxBodyBytes bounds the stored request body.
	tinybirdMaxBodyBytes = 2 << 20
	// tinybirdMaxEncodedBodyBytes bounds the raw bytes held for a compressed body so it can still be decoded.
	tinybirdMaxEncodedBodyBytes = 8 << 20
)

// TinybirdLoggingMiddleware captures every request (headers and body, with credentials redacted)
// and hands it to the sink. The body is read up front and replayed to the handler,
// and decoding and delivery happen off the request path. A nil sink disables the middleware.
func TinybirdLoggingMiddleware(sink *tinybirdlog.Sink) gin.HandlerFunc {
	return func(c *gin.Context) {
		if sink == nil || !shouldLogRequest(c.Request.URL.Path) {
			c.Next()
			return
		}

		start := time.Now()
		encoding := strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
		// Read the leading bytes up front and replay them, so bodies are captured even when
		// the request is rejected (for example by authentication) before any handler reads it.
		var raw []byte
		truncated := false
		if c.Request.Body != nil && c.Request.Body != http.NoBody && c.Request.ContentLength != 0 {
			limit := tinybirdMaxBodyBytes
			if encoding != "" && !strings.EqualFold(encoding, "identity") {
				limit = tinybirdMaxEncodedBodyBytes
			}
			original := c.Request.Body
			head, errRead := io.ReadAll(io.LimitReader(original, int64(limit)))
			raw = head
			truncated = len(head) == limit
			replay := io.MultiReader(bytes.NewReader(head), original)
			if errRead != nil {
				replay = io.MultiReader(bytes.NewReader(head), errReader{errRead})
			}
			c.Request.Body = replayedBody{Reader: replay, Closer: original}
		}

		c.Next()

		event := &tinybirdlog.Event{
			Timestamp:      start,
			RequestID:      logging.GetGinRequestID(c),
			Method:         c.Request.Method,
			Path:           c.Request.URL.Path,
			Query:          util.MaskSensitiveQuery(c.Request.URL.RawQuery),
			Status:         c.Writer.Status(),
			DurationMs:     time.Since(start).Milliseconds(),
			ClientIP:       c.ClientIP(),
			UserAgent:      c.Request.UserAgent(),
			RequestHeaders: c.Request.Header.Clone(),
		}
		event.RequestBodySize = int64(len(raw))
		event.RequestBodyTrunc = truncated
		if truncated && c.Request.ContentLength > 0 {
			event.RequestBodySize = c.Request.ContentLength
		}

		// Decoding a compressed body can be slow, so finish the event outside the request path.
		sink.Async(func() {
			if len(raw) > 0 {
				decoded := decodeCapturedRequestBodyForLogWithLimit(raw, encoding, tinybirdMaxBodyBytes)
				if len(decoded) > tinybirdMaxBodyBytes {
					decoded = decoded[:tinybirdMaxBodyBytes]
					event.RequestBodyTrunc = true
				}
				event.RequestBody = decoded
			}
			sink.Enqueue(event.Encode())
		})
	}
}

// replayedBody serves the pre-read bytes followed by the rest of the original body.
type replayedBody struct {
	io.Reader
	io.Closer
}

// errReader returns a read error that happened while the body was pre-read.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
