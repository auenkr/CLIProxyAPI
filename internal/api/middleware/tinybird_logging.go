package middleware

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
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

		capture := &responseCapture{ResponseWriter: c.Writer, websocketUpgrade: websocket.IsWebSocketUpgrade(c.Request)}
		c.Writer = capture

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
				decoded := decodeCapturedRequestBodyForLogWithLimit(raw, encoding, tinybirdMaxBodyBytes)
				if len(decoded) > tinybirdMaxBodyBytes {
					decoded = decoded[:tinybirdMaxBodyBytes]
					event.RequestBodyTrunc = true
				}
				event.RequestBody = decoded
			}
			if len(responseRaw) > 0 {
				decoded := decodeCapturedRequestBodyForLogWithLimit(responseRaw, responseEncoding, tinybirdMaxBodyBytes)
				if len(decoded) > tinybirdMaxBodyBytes {
					decoded = decoded[:tinybirdMaxBodyBytes]
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
	body             bytes.Buffer
	total            int64
	truncated        bool
	websocketUpgrade bool
	hijacked         bool
}

// Status reports a successful WebSocket hijack as an upgrade. Other responses
// retain the underlying writer's status semantics.
func (w *responseCapture) Status() int {
	if w.hijacked && w.websocketUpgrade {
		return http.StatusSwitchingProtocols
	}
	return w.ResponseWriter.Status()
}

// Hijack preserves the connection handoff and records it only after success.
func (w *responseCapture) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, rw, err
}

func (w *responseCapture) record(data []byte) {
	w.total += int64(len(data))
	room := tinybirdMaxBodyBytes - w.body.Len()
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
