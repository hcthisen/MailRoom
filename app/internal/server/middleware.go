package server

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"

	"mailroom/internal/core"
)

// securityHeaders sets baseline security headers. Handlers may override
// them (e.g. a hosted form page relaxing form-action for its redirect),
// because the headers are set before the handler runs.
func securityHeaders(hsts bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code and body size for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush supports streaming handlers.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack supports protocol upgrades.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("server: hijacking not supported")
}

// requestLogger logs one line per request. Paths are redacted so tokens in
// public URLs never reach the logs; query strings are never logged.
func requestLogger(log *slog.Logger, next http.Handler) http.Handler {
	clock := core.SystemClock{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := clock.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		level := slog.LevelInfo
		switch {
		case r.URL.Path == "/healthz":
			level = slog.LevelDebug
		case rec.status >= 500:
			level = slog.LevelError
		}
		log.LogAttrs(r.Context(), level, "http request",
			slog.String("method", r.Method),
			slog.String("path", RedactPath(r.URL.Path)),
			slog.Int("status", rec.status),
			slog.Int64("bytes", rec.bytes),
			slog.Duration("duration", clock.Now().Sub(start)),
			slog.String("remote", clientIP(r)),
		)
	})
}

// tokenPrefixes are public paths whose next segment is a secret token.
var tokenPrefixes = []string{"/u/", "/c/", "/o/"}

// RedactPath replaces secret-bearing path segments with "_": the segment
// after /u/, /c/ and /o/, and any segment of 32 or more characters (long
// random tokens, e.g. opt-in confirmation links).
func RedactPath(p string) string {
	for _, pre := range tokenPrefixes {
		if strings.HasPrefix(p, pre) && len(p) > len(pre) {
			rest := p[len(pre):]
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				return pre + "_" + RedactPath(rest[i:])
			}
			return pre + "_"
		}
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if len(s) >= 32 {
			segs[i] = "_"
		}
	}
	return strings.Join(segs, "/")
}

// clientIP returns the request's remote IP (without port).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// recoverer turns a handler panic into a logged 500 instead of a dropped
// connection. http.ErrAbortHandler is re-raised, as net/http expects.
func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler {
				panic(p)
			}
			log.Error("http handler panic",
				"method", r.Method, "path", RedactPath(r.URL.Path),
				"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"internal server error"}}`))
		}()
		next.ServeHTTP(w, r)
	})
}
