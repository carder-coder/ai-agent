package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/trace"
)

const (
	idempotencyTTL           = 24 * time.Hour
	maxIdempotencyEntries    = 10000
	maxIdempotencyBodyBytes  = 256 << 10
	maxIdempotencyCacheBytes = 32 << 20
)

type cachedResponse struct {
	status     int
	headers    http.Header
	body       []byte
	replayable bool
}

type idempotencyEntry struct {
	fingerprint string
	done        chan struct{}
	expiresAt   time.Time
	response    cachedResponse
}

type IdempotencyCache struct {
	mu         sync.Mutex
	entries    map[string]*idempotencyEntry
	cachedSize int
}

func NewIdempotencyCache() *IdempotencyCache {
	return &IdempotencyCache{entries: make(map[string]*idempotencyEntry)}
}

func (c *IdempotencyCache) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" || r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			next.ServeHTTP(w, r)
			return
		}
		if !validIdempotencyKey(key) {
			writeError(w, http.StatusBadRequest, "Idempotency-Key: must be 1-255 visible ASCII characters", "invalid_request_error", "invalid_idempotency_key")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "body: unable to read request", "invalid_request_error", "invalid_request")
			return
		}
		if len(body) > maxRequestBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "body: exceeds 1 MiB limit", "invalid_request_error", "request_too_large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		fingerprint := requestFingerprint(r, body)
		scope := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		cacheKey := hex.EncodeToString(scope[:]) + ":" + key
		c.mu.Lock()
		c.purgeExpiredLocked(time.Now())
		entry, exists := c.entries[cacheKey]
		if exists {
			if entry.fingerprint != fingerprint {
				c.mu.Unlock()
				writeError(w, http.StatusConflict, "Idempotency-Key was already used with a different request", "invalid_request_error", "idempotency_key_reused")
				return
			}
			c.mu.Unlock()
			select {
			case <-r.Context().Done():
				return
			case <-entry.done:
				if !entry.response.replayable {
					writeError(w, http.StatusConflict, "original response is too large to replay; request was not repeated", "invalid_request_error", "idempotency_result_not_replayable")
					return
				}
				replayResponse(w, entry.response)
			}
			return
		}
		if len(c.entries) >= maxIdempotencyEntries {
			c.mu.Unlock()
			writeError(w, http.StatusServiceUnavailable, "idempotency cache is full; request was not sent", "server_error", "idempotency_cache_full")
			return
		}
		entry = &idempotencyEntry{
			fingerprint: fingerprint,
			done:        make(chan struct{}),
			expiresAt:   time.Now().Add(idempotencyTTL),
		}
		c.entries[cacheKey] = entry
		c.mu.Unlock()

		capture := &captureWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
			maxBytes:       maxIdempotencyBodyBytes,
			cacheable:      true,
		}
		var responseWriter http.ResponseWriter = capture
		if flusher, ok := w.(http.Flusher); ok {
			responseWriter = &flushCaptureWriter{captureWriter: capture, flusher: flusher}
		}
		next.ServeHTTP(responseWriter, r)

		headers := make(http.Header)
		for _, name := range []string{"Content-Type", "Retry-After"} {
			for _, value := range capture.Header().Values(name) {
				headers.Add(name, value)
			}
		}
		snapshot := cachedResponse{
			status:     capture.status,
			headers:    headers,
			body:       capture.body,
			replayable: capture.cacheable && r.Context().Err() == nil,
		}
		c.mu.Lock()
		if snapshot.replayable && c.cachedSize+len(snapshot.body) <= maxIdempotencyCacheBytes {
			c.cachedSize += len(snapshot.body)
		} else {
			snapshot.replayable = false
			snapshot.body = nil
		}
		entry.response = snapshot
		entry.expiresAt = time.Now().Add(idempotencyTTL)
		close(entry.done)
		c.mu.Unlock()
	})
}

func (c *IdempotencyCache) purgeExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		select {
		case <-entry.done:
			if !now.Before(entry.expiresAt) {
				c.cachedSize -= len(entry.response.body)
				delete(c.entries, key)
			}
		default:
		}
	}
}

func validIdempotencyKey(key string) bool {
	if len(key) == 0 || len(key) > 255 {
		return false
	}
	for _, value := range []byte(key) {
		if value < 0x21 || value > 0x7e {
			return false
		}
	}
	return true
}

func requestFingerprint(r *http.Request, body []byte) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\n%s\n", r.Method, r.URL.RequestURI())
	_, _ = hash.Write(body)
	return hex.EncodeToString(hash.Sum(nil))
}

func replayResponse(w http.ResponseWriter, response cachedResponse) {
	for name, values := range response.headers {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.Header().Set("Idempotency-Replayed", "true")
	w.WriteHeader(response.status)
	if _, err := w.Write(response.body); err != nil {
		log.Printf("重放幂等响应失败：%v", err)
	}
}

type captureWriter struct {
	http.ResponseWriter
	status    int
	wrote     bool
	body      []byte
	maxBytes  int
	cacheable bool
}

func (w *captureWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *captureWriter) Write(body []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	if w.cacheable {
		if len(w.body)+n > w.maxBytes {
			w.body = nil
			w.cacheable = false
		} else if n > 0 {
			w.body = append(w.body, body[:n]...)
		}
	}
	return n, err
}

func (w *captureWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

type flushCaptureWriter struct {
	*captureWriter
	flusher http.Flusher
}

func (w *flushCaptureWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	w.flusher.Flush()
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responseWriter, tracked := trackedRecoveryWriter(w)
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("网关请求 panic：%v\n%s", recovered, debug.Stack())
				if !tracked.wroteHeader {
					writeError(tracked, http.StatusInternalServerError, "internal server error", "server_error", "internal_error")
				} else if strings.HasPrefix(tracked.Header().Get("Content-Type"), "text/event-stream") {
					_, _ = io.WriteString(tracked, "data: {\"error\":{\"message\":\"internal server error\",\"type\":\"server_error\",\"code\":\"internal_error\"}}\n\ndata: [DONE]\n\n")
					if flusher, ok := responseWriter.(http.Flusher); ok {
						flusher.Flush()
					}
				}
			}
		}()
		next.ServeHTTP(responseWriter, r)
	})
}

func requestLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := trace.NewID()
		if err != nil {
			log.Printf("生成 trace_id 失败：%v", err)
			writeError(w, http.StatusInternalServerError, "failed to generate trace ID", "server_error", "internal_error")
			return
		}
		w.Header().Set("X-Trace-ID", id)
		trackedWriter, tracked := trackedRecoveryWriter(w)
		started := time.Now()
		next.ServeHTTP(trackedWriter, r.WithContext(trace.WithID(r.Context(), id)))
		log.Printf(
			"request method=%s path=%s status=%d duration_ms=%d trace_id=%s",
			r.Method,
			r.URL.Path,
			tracked.status,
			time.Since(started).Milliseconds(),
			id,
		)
	})
}

func concurrencyMiddleware(limit int, next http.Handler) http.Handler {
	semaphore := make(chan struct{}, limit)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case semaphore <- struct{}{}:
			defer func() { <-semaphore }()
			next.ServeHTTP(w, r)
		default:
			writeError(w, http.StatusServiceUnavailable, "gateway concurrency limit reached", "server_error", "concurrency_limit")
		}
	})
}

func trackedRecoveryWriter(w http.ResponseWriter) (http.ResponseWriter, *recoveryWriter) {
	tracked := &recoveryWriter{ResponseWriter: w, status: http.StatusOK}
	if flusher, ok := w.(http.Flusher); ok {
		return &flushRecoveryWriter{recoveryWriter: tracked, flusher: flusher}, tracked
	}
	return tracked, tracked
}

type recoveryWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *recoveryWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *recoveryWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *recoveryWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

type flushRecoveryWriter struct {
	*recoveryWriter
	flusher http.Flusher
}

func (w *flushRecoveryWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.flusher.Flush()
}
