package logging

import (
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
		remote := strings.TrimSpace(r.RemoteAddr)
		if host, _, err := net.SplitHostPort(remote); err == nil {
			remote = host
		}
		user := "-"
		if u := strings.TrimSpace(r.URL.User.Username()); u != "" {
			user = u
		}
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

func PageLogLine(jobID int64, user, printer, title string, copies int, result string) string {
	if copies <= 0 {
		copies = 1
	}
	if strings.TrimSpace(result) == "" {
		result = "ok"
	}
	if strings.TrimSpace(user) == "" {
		user = "-"
	}
	if strings.TrimSpace(printer) == "" {
		printer = "-"
	}
	if strings.TrimSpace(title) == "" {
		title = "Untitled"
	}
	return strings.Join([]string{
		printer,
		user,
		strconv.FormatInt(jobID, 10),
		time.Now().Format(time.RFC3339),
		title,
		strconv.Itoa(copies),
		result,
	}, " ")
}
