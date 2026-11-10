package middleware

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouselog"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const (
	// clickhouseMaxBodyBytes bounds the stored request body.
	clickhouseMaxBodyBytes = 2 << 20
	// clickhouseMaxEncodedBodyBytes bounds the raw bytes held for a compressed body so it can still be decoded.
	clickhouseMaxEncodedBodyBytes = 8 << 20
)

// ClickHouseLoggingMiddleware captures every request (headers and body, with credentials redacted)
// and hands it to the sink. The body is read up front and replayed to the handler,
// and decoding and delivery happen off the request path. A nil sink disables the middleware.
func ClickHouseLoggingMiddleware(sink *clickhouselog.Sink) gin.HandlerFunc {
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
			limit := clickhouseMaxBodyBytes
			if encoding != "" && !strings.EqualFold(encoding, "identity") {
				limit = clickhouseMaxEncodedBodyBytes
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

		capture := &responseCapture{ResponseWriter: c.Writer}
		c.Writer = capture

		c.Next()

		event := &clickhouselog.Event{
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
		event.ResponseHeaders = c.Writer.Header().Clone()
		responseRaw := capture.body.Bytes()
		event.ResponseBodySize = capture.total
		event.ResponseBodyTrunc = capture.truncated
		responseEncoding := strings.TrimSpace(event.ResponseHeaders.Get("Content-Encoding"))
		event.RequestBodySize = int64(len(raw))
		event.RequestBodyTrunc = truncated
		if truncated && c.Request.ContentLength > 0 {
			event.RequestBodySize = c.Request.ContentLength
		}

		// Decoding a compressed body can be slow, so finish the event outside the request path.
		sink.Async(func() {
			if len(raw) > 0 {
				decoded := decodeCapturedRequestBodyForLogWithLimit(raw, encoding, clickhouseMaxBodyBytes)
				if len(decoded) > clickhouseMaxBodyBytes {
					decoded = decoded[:clickhouseMaxBodyBytes]
					event.RequestBodyTrunc = true
				}
				event.RequestBody = decoded
			}
			if len(responseRaw) > 0 {
				decoded := decodeCapturedRequestBodyForLogWithLimit(responseRaw, responseEncoding, clickhouseMaxBodyBytes)
				if len(decoded) > clickhouseMaxBodyBytes {
					decoded = decoded[:clickhouseMaxBodyBytes]
					event.ResponseBodyTrunc = true
				}
				event.ResponseBody = decoded
			}
			sink.Enqueue(event.Encode())
		})
	}
}

// responseCapture copies the leading bytes written to the client without altering the
// response. It embeds gin.ResponseWriter so Flush, Hijack and the other optional
// interfaces keep working for streaming and websocket handlers.
type responseCapture struct {
	gin.ResponseWriter
	body      bytes.Buffer
	total     int64
	truncated bool
}

func (w *responseCapture) record(data []byte) {
	w.total += int64(len(data))
	room := clickhouseMaxBodyBytes - w.body.Len()
	if room <= 0 {
		w.truncated = w.truncated || len(data) > 0
		return
	}
	if len(data) > room {
		data = data[:room]
		w.truncated = true
	}
	w.body.Write(data)
}

func (w *responseCapture) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.record(data[:n])
	return n, err
}

func (w *responseCapture) WriteString(data string) (int, error) {
	n, err := w.ResponseWriter.WriteString(data)
	w.record([]byte(data[:n]))
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *responseCapture) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// replayedBody serves the pre-read bytes followed by the rest of the original body.
type replayedBody struct {
	io.Reader
	io.Closer
}

// errReader returns a read error that happened while the body was pre-read.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
