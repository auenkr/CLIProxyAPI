package clickhouselog

import (
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MoeForge/moego/ojson"
)

// Redacted replaces the value of every sensitive header.
const Redacted = "[REDACTED]"

// sensitiveHeaders are never stored; everything else is kept verbatim.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"x-goog-api-key":      true,
	"cookie":              true,
	"set-cookie":          true,
}

// Event is one captured request, stored as one row of the request table.
type Event struct {
	Timestamp        time.Time
	RequestID        string
	Method           string
	Path             string
	Query            string
	Status           int
	DurationMs       int64
	ClientIP         string
	UserAgent        string
	RequestHeaders   http.Header
	RequestBody      []byte
	RequestBodySize  int64
	RequestBodyTrunc bool

	ResponseHeaders   http.Header
	ResponseBody      []byte
	ResponseBodySize  int64
	ResponseBodyTrunc bool
}

// HeadersJSON serialises headers as a JSON object of name to value list, redacting sensitive ones.
func HeadersJSON(headers http.Header) string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)

	buf := make([]byte, 0, 512)
	buf = append(buf, '{')
	for i, name := range names {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = ojson.AppendJSONString(buf, name)
		buf = append(buf, ':', '[')
		values := headers[name]
		if sensitiveHeaders[strings.ToLower(name)] {
			values = []string{Redacted}
		}
		for j, value := range values {
			if j > 0 {
				buf = append(buf, ',')
			}
			if !utf8.ValidString(value) {
				value = strings.ToValidUTF8(value, "�")
			}
			buf = ojson.AppendJSONString(buf, value)
		}
		buf = append(buf, ']')
	}
	buf = append(buf, '}')
	return string(buf)
}

// columns lists the insert columns in the order produced by row.
var columns = []string{
	"timestamp", "request_id", "method", "path", "query", "status", "duration_ms",
	"client_ip", "user_agent", "request_headers", "request_body", "request_body_bytes",
	"request_body_truncated", "response_headers", "response_body", "response_body_bytes",
	"response_body_truncated",
}

func flag(v bool) uint8 {
	if v {
		return 1
	}
	return 0
}

// clampU32 converts a non-negative size to the UInt32 column type.
func clampU32(v int64) uint32 {
	switch {
	case v < 0:
		return 0
	case v > int64(^uint32(0)):
		return ^uint32(0)
	default:
		return uint32(v)
	}
}

// row returns the column values in the order of columns.
func (e *Event) row() []any {
	status := e.Status
	if status < 0 || status > 65535 {
		status = 0
	}
	return []any{
		e.Timestamp.UTC(),
		e.RequestID,
		e.Method,
		e.Path,
		e.Query,
		uint16(status),
		clampU32(e.DurationMs),
		e.ClientIP,
		e.UserAgent,
		HeadersJSON(e.RequestHeaders),
		strings.ToValidUTF8(string(e.RequestBody), "\uFFFD"),
		clampU32(e.RequestBodySize),
		flag(e.RequestBodyTrunc),
		HeadersJSON(e.ResponseHeaders),
		strings.ToValidUTF8(string(e.ResponseBody), "\uFFFD"),
		clampU32(e.ResponseBodySize),
		flag(e.ResponseBodyTrunc),
	}
}

// size approximates the memory held by the event for queue accounting.
func (e *Event) size() int64 {
	return int64(len(e.RequestBody)+len(e.ResponseBody)+len(e.Path)+len(e.Query)+len(e.UserAgent)) + 1024
}
