package tinybirdlog

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

// Event is one captured request.
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
}

// Encode renders the event as one NDJSON line matching the proxy_requests schema.
func (e *Event) Encode() []byte {
	doc := ojson.NewDocString("{}")
	_ = doc.Set("timestamp", e.Timestamp.UTC().Format("2006-01-02 15:04:05.000"))
	_ = doc.Set("request_id", e.RequestID)
	_ = doc.Set("method", e.Method)
	_ = doc.Set("path", e.Path)
	_ = doc.Set("query", e.Query)
	_ = doc.Set("status", e.Status)
	_ = doc.Set("duration_ms", e.DurationMs)
	_ = doc.Set("client_ip", e.ClientIP)
	_ = doc.Set("user_agent", e.UserAgent)
	_ = doc.Set("request_headers", HeadersJSON(e.RequestHeaders))
	_ = doc.Set("request_body", strings.ToValidUTF8(string(e.RequestBody), "�"))
	_ = doc.Set("request_body_bytes", e.RequestBodySize)
	truncated := 0
	if e.RequestBodyTrunc {
		truncated = 1
	}
	_ = doc.Set("request_body_truncated", truncated)
	return doc.Bytes()
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
