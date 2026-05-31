package main

import (
	"log"
	"net/http"
	"strings"
	"time"
)

// loggingResponseWriter wraps http.ResponseWriter to capture the status code
// while transparently forwarding Flush, so SSE streaming keeps working.
type loggingResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *loggingResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer when it supports streaming (SSE).
// Without this, wrapping the writer would hide http.Flusher and break /sse.
func (w *loggingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// loggingMiddleware logs one line per request: method, path, status, duration,
// client IP. Health checks are skipped to avoid noise; the long-lived SSE
// stream is logged both on connect and on disconnect.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		lw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		if r.URL.Path == "/sse" {
			log.Printf("[http] %s %s open (client=%s)", r.Method, r.URL.Path, clientIP(r))
		}
		next.ServeHTTP(lw, r)
		log.Printf("[http] %s %s %d %s (client=%s)",
			r.Method, r.URL.Path, lw.status, time.Since(start).Round(time.Millisecond), clientIP(r))
	})
}

// clientIP extracts the real client address, preferring the headers nginx sets
// when it reverse-proxies (the connection itself comes from 127.0.0.1).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return xr
	}
	return r.RemoteAddr
}
