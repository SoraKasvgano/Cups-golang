package logging

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type responseRecorder struct {
	http.ResponseWriter
	status int
	size   int
}

func (r *responseRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.size += n
	return n, err
}

func HTTPAccessMiddleware(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		if !shouldLogAccess(AccessLogLevel(), r.Method, r.URL.Path, status) {
			return
		}
		remote := strings.TrimSpace(r.RemoteAddr)
		if host, _, err := net.SplitHostPort(remote); err == nil {
			remote = host
		}
		user := parseAuthUser(r)
		line := fmt.Sprintf("%s - %s [%s] \"%s %s %s\" %d %d",
			remote,
			user,
			start.Format("02/Jan/2006:15:04:05 -0700"),
			r.Method,
			r.URL.RequestURI(),
			r.Proto,
			status,
			rec.size,
		)
		Access(line)
	})
}

type PageLogEntry struct {
	JobID      int64
	User       string
	Printer    string
	Title      string
	Copies     int
	PageNumber string
	OriginHost string
	Media      string
	Sides      string
	Billing    string
	AccountID  string
	Extra      map[string]string
}

func PageLogLine(entry PageLogEntry) string {
	format := strings.TrimSpace(PageLogFormat())
	if format == "" {
		return ""
	}
	return formatPageLogLine(format, entry)
}

func shouldLogAccess(level, method, path string, status int) bool {
	level = normalizeAccessLogLevel(level)
	method = strings.ToUpper(strings.TrimSpace(method))
	switch level {
	case "all":
		return true
	case "config":
		if status >= 400 {
			return true
		}
		if strings.HasPrefix(path, "/admin") {
			return true
		}
		return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
	default: // actions
		if status >= 400 {
			return true
		}
		return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
	}
}

func parseAuthUser(r *http.Request) string {
	if r == nil {
		return "-"
	}
	if u := strings.TrimSpace(r.URL.User.Username()); u != "" {
		return u
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return "-"
	}
	switch {
	case strings.HasPrefix(strings.ToLower(auth), "basic "):
		payload := strings.TrimSpace(auth[len("Basic "):])
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return "-"
		}
		user, _, found := strings.Cut(string(decoded), ":")
		if !found {
			return "-"
		}
		user = strings.TrimSpace(user)
		if user == "" {
			return "-"
		}
		return user
	case strings.HasPrefix(strings.ToLower(auth), "digest "):
		user := parseDigestUsername(auth[len("Digest "):])
		if user == "" {
			return "-"
		}
		return user
	default:
		return "-"
	}
}

func parseDigestUsername(value string) string {
	for _, field := range strings.Split(value, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		key, raw, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(key), "username") {
			continue
		}
		raw = strings.TrimSpace(raw)
		raw = strings.Trim(raw, "\"")
		return strings.TrimSpace(raw)
	}
	return ""
}

func formatPageLogLine(format string, entry PageLogEntry) string {
	copies := entry.Copies
	if copies <= 0 {
		copies = 1
	}
	pageNumber := strings.TrimSpace(entry.PageNumber)
	if pageNumber == "" {
		pageNumber = "1"
	}
	repl := func(name string) string {
		switch name {
		case "p":
			return valueOrDash(entry.Printer)
		case "u":
			return valueOrDash(entry.User)
		case "j":
			if entry.JobID <= 0 {
				return "-"
			}
			return strconv.FormatInt(entry.JobID, 10)
		case "T":
			return time.Now().Format("[02/Jan/2006:15:04:05 -0700]")
		case "P":
			return valueOrDash(pageNumber)
		case "C":
			return strconv.Itoa(copies)
		default:
			return "%" + name
		}
	}

	var b strings.Builder
	for i := 0; i < len(format); i++ {
		ch := format[i]
		if ch != '%' {
			b.WriteByte(ch)
			continue
		}
		if i+1 >= len(format) {
			b.WriteByte('%')
			continue
		}
		next := format[i+1]
		i++
		switch next {
		case '%':
			b.WriteByte('%')
		case '{':
			start := i + 1
			end := strings.IndexByte(format[start:], '}')
			if end < 0 {
				b.WriteString("%{")
				continue
			}
			name := strings.TrimSpace(format[start : start+end])
			i = start + end
			b.WriteString(resolvePageAttribute(entry, name))
		default:
			b.WriteString(repl(string(next)))
		}
	}
	return strings.TrimSpace(b.String())
}

func resolvePageAttribute(entry PageLogEntry, name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "job-billing":
		if v := strings.TrimSpace(entry.Billing); v != "" {
			return v
		}
		if v := strings.TrimSpace(entry.AccountID); v != "" {
			return v
		}
		return "-"
	case "job-account-id":
		if v := strings.TrimSpace(entry.AccountID); v != "" {
			return v
		}
		return "-"
	case "job-originating-host-name":
		return valueOrDash(entry.OriginHost)
	case "job-name":
		return valueOrDash(entry.Title)
	case "media":
		return valueOrDash(entry.Media)
	case "sides":
		return valueOrDash(entry.Sides)
	default:
		if entry.Extra != nil {
			if v := strings.TrimSpace(entry.Extra[name]); v != "" {
				return v
			}
		}
		return "-"
	}
}

func valueOrDash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}
