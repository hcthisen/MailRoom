package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Idempotency-Key handling (G-6).
//
// A POST under /api/v1 carrying an Idempotency-Key header is executed at
// most once per (actor, key) within IdempotencyTTL:
//
//   - The key is recorded, with a hash of the request (method, path, query
//     and body), inside the first write transaction the request commits
//     (through db.WithCommitHook, so every service's db.WithTx joins in).
//     The side effects and the key commit together: a crash or failed write
//     after the commit can't lead to a second execution.
//   - When the request finishes, its status, content type and body are
//     stored with the key. A later request with the same key and the same
//     hash gets that response replayed, with "Idempotent-Replayed: true".
//   - The same key with a different request is refused with 422, also
//     after a failed attempt.
//   - While the first request is still running, a concurrent request with
//     the same key is refused with 409 conflict (retry after a moment).
//   - A request that failed without committing anything (a validation
//     error, a rolled-back transaction) may be retried with the same key and
//     the same request: it simply runs again.
//   - When the response could not be kept (operations marked MetaSensitive,
//     which return a one-time secret, responses over 1 MiB, or a server that
//     stopped right after the commit), a retry is refused with 409: the
//     operation was carried out; read the resulting state instead.
//
// The remaining window: a process killed after the commit but before the
// response was stored leaves only the key, so the client gets the 409 above
// rather than the original response.
const (
	IdempotencyHeader        = "Idempotency-Key"
	IdempotencyReplayHeader  = "Idempotent-Replayed"
	IdempotencyTTL           = 24 * time.Hour
	MaxIdempotencyKeyLength  = 255
	maxStoredResponseBytes   = 1 << 20  // larger responses are not stored
	maxHashedRequestBodySize = 64 << 20 // larger bodies are not tracked
)

// Special idempotency_keys.status_code values; real responses are > 0.
const (
	// keyCommitted: the request committed its changes; no response stored.
	keyCommitted = 0
	// keyFailed: the request failed without committing; it may be re-run
	// with the same request, and the hash guards against key reuse.
	keyFailed = -1
)

// idempotency is the Idempotency-Key middleware and its store.
type idempotency struct {
	db    *db.DB
	clock core.Clock
	log   *slog.Logger

	mu       sync.Mutex
	inflight map[string]struct{}
}

func newIdempotency(d *db.DB, clock core.Clock, log *slog.Logger) *idempotency {
	return &idempotency{db: d, clock: clock, log: log, inflight: map[string]struct{}{}}
}

type storedResponse struct {
	requestHash string
	status      int
	body        []byte
	contentType string
}

func validIdempotencyKey(k string) bool {
	if k == "" || len(k) > MaxIdempotencyKeyLength {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

func keyError(status int, code, msg string) *Error {
	return NewError(status, code, msg, "header."+IdempotencyHeader)
}

// wrap applies the Idempotency-Key semantics to POST requests. It must run
// after authentication, because keys are scoped per actor.
func (m *idempotency) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(IdempotencyHeader)
		if r.Method != http.MethodPost || key == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !validIdempotencyKey(key) {
			keyError(http.StatusUnprocessableEntity, CodeValidation,
				fmt.Sprintf("must be 1-%d visible ASCII characters", MaxIdempotencyKeyLength)).Write(w)
			return
		}
		p, ok := PrincipalFromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		actor := p.Actor.String()
		slot := actor + "\x00" + key
		if !m.acquire(slot) {
			keyError(http.StatusConflict, CodeConflict,
				"a request with this Idempotency-Key is still in progress; retry shortly").Write(w)
			return
		}
		defer m.release(slot)

		ctx := r.Context()
		prev, found, err := m.lookup(ctx, actor, key)
		if err != nil {
			m.log.ErrorContext(ctx, "api: idempotency lookup", "err", err)
			NewError(http.StatusInternalServerError, CodeInternal, internalMessage, "").Write(w)
			return
		}
		body := &hashingBody{ReadCloser: r.Body, h: requestHasher(r)}
		if found {
			// Hash the whole request to compare it; keep a copy in case a
			// failed attempt runs again.
			var buf bytes.Buffer
			n, err := io.Copy(&buf, io.LimitReader(body, maxHashedRequestBodySize+1))
			if err != nil {
				NewError(http.StatusBadRequest, CodeValidation, "could not read the request body", "").Write(w)
				return
			}
			if n > maxHashedRequestBodySize || body.sum() != prev.requestHash {
				keyError(http.StatusUnprocessableEntity, CodeValidation,
					"this Idempotency-Key was already used for a different request").Write(w)
				return
			}
			switch {
			case prev.status > 0:
				w.Header().Set("Content-Type", prev.contentType)
				w.Header().Set(IdempotencyReplayHeader, "true")
				w.WriteHeader(prev.status)
				_, _ = w.Write(prev.body)
				return
			case prev.status == keyCommitted:
				keyError(http.StatusConflict, CodeConflict,
					"the request with this Idempotency-Key was already carried out, but its response was not kept; "+
						"read the current state instead of retrying").Write(w)
				return
			}
			// keyFailed: run it again with the buffered body.
			body = &hashingBody{ReadCloser: io.NopCloser(&buf), h: sha256.New(), fixed: prev.requestHash}
		}
		m.execute(w, r, next, actor, key, body)
	})
}

// execute runs next with the key recorded in its first committed
// transaction, then stores the outcome.
func (m *idempotency) execute(w http.ResponseWriter, r *http.Request, next http.Handler, actor, key string, body *hashingBody) {
	ctx := r.Context()
	var committed atomic.Bool
	hook := &db.CommitHook{
		Before: func(ctx context.Context, tx *sql.Tx) error {
			return m.reserve(ctx, tx, actor, key, r, body.sum())
		},
		After: func() { committed.Store(true) },
	}
	r.Body = body
	r = r.WithContext(db.WithCommitHook(ctx, hook))
	rec := &captureWriter{ResponseWriter: w}
	next.ServeHTTP(rec, r)

	// Hash whatever the handler didn't read, so the hash covers the body.
	// Usually the handler already closed the body, and Close drained it.
	tracked := body.drain() == nil && body.bytesRead() <= maxHashedRequestBodySize
	storable := tracked && !rec.overflow && !strings.Contains(rec.Header().Get("Cache-Control"), "no-store")
	ct := rec.Header().Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	resp := storedResponse{requestHash: body.sum(), status: rec.status, body: rec.buf.Bytes(), contentType: ct}
	if resp.status == 0 {
		resp.status = http.StatusOK
	}
	sctx := context.WithoutCancel(ctx)
	var err error
	switch {
	case committed.Load() && storable && resp.status < 500:
		err = m.store(sctx, actor, key, r, resp)
	case committed.Load() && tracked:
		// Keep the committed marker, with the hash of the whole body. A 5xx
		// after a commit isn't replayed: the change did happen.
		resp.status, resp.body = keyCommitted, nil
		err = m.store(sctx, actor, key, r, resp)
	case committed.Load():
		// The marker from the commit stays as it is.
	case storable && resp.status >= 200 && resp.status <= 299:
		err = m.store(sctx, actor, key, r, resp) // nothing committed, e.g. a dry run or preview
	case tracked:
		resp.status, resp.body = keyFailed, nil
		err = m.store(sctx, actor, key, r, resp)
	}
	if err != nil {
		// The response is already sent. When the request committed, the
		// marker written with it still blocks a second execution.
		m.log.ErrorContext(ctx, "api: idempotency store", "err", err)
	}
}

func (m *idempotency) acquire(slot string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, busy := m.inflight[slot]; busy {
		return false
	}
	m.inflight[slot] = struct{}{}
	return true
}

func (m *idempotency) release(slot string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inflight, slot)
}

func (m *idempotency) cutoff() string {
	return db.FormatTime(m.clock.Now().Add(-IdempotencyTTL))
}

func (m *idempotency) lookup(ctx context.Context, actor, key string) (storedResponse, bool, error) {
	var s storedResponse
	err := m.db.Read.QueryRowContext(ctx,
		`SELECT request_hash, status_code, response_body, response_type FROM idempotency_keys
		 WHERE actor = ? AND key = ? AND created_at >= ?`, actor, key, m.cutoff()).
		Scan(&s.requestHash, &s.status, &s.body, &s.contentType)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("idempotency lookup: %w", err)
	}
	return s, true, nil
}

// reserve records the key as committed inside tx. It replaces an expired
// row or a failed attempt, and leaves a row this request already wrote (a
// second transaction of the same request) alone.
func (m *idempotency) reserve(ctx context.Context, tx *sql.Tx, actor, key string, r *http.Request, requestHash string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO idempotency_keys (actor, key, method, path, request_hash, status_code, response_body, response_type, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, x'', 'application/json', ?)
		 ON CONFLICT (actor, key) DO UPDATE SET method = excluded.method, path = excluded.path,
		   request_hash = excluded.request_hash, status_code = excluded.status_code,
		   response_body = excluded.response_body, response_type = excluded.response_type,
		   created_at = excluded.created_at
		 WHERE idempotency_keys.status_code = ? OR idempotency_keys.created_at < ?`,
		actor, key, r.Method, r.URL.Path, requestHash, keyCommitted, db.FormatTime(m.clock.Now()), keyFailed, m.cutoff())
	if err != nil {
		return fmt.Errorf("idempotency reserve: %w", err)
	}
	return nil
}

// store saves the outcome for the key, replacing any earlier row.
func (m *idempotency) store(ctx context.Context, actor, key string, r *http.Request, s storedResponse) error {
	if s.body == nil {
		s.body = []byte{}
	}
	_, err := m.db.Write.ExecContext(ctx,
		`INSERT INTO idempotency_keys (actor, key, method, path, request_hash, status_code, response_body, response_type, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (actor, key) DO UPDATE SET method = excluded.method, path = excluded.path,
		   request_hash = excluded.request_hash, status_code = excluded.status_code,
		   response_body = excluded.response_body, response_type = excluded.response_type,
		   created_at = excluded.created_at`,
		actor, key, r.Method, r.URL.Path, s.requestHash, s.status, s.body, s.contentType, db.FormatTime(m.clock.Now()))
	if err != nil {
		return fmt.Errorf("idempotency store: %w", err)
	}
	return nil
}

// cleanup deletes keys older than IdempotencyTTL.
func (m *idempotency) cleanup(ctx context.Context) (int64, error) {
	res, err := m.db.Write.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < ?`, m.cutoff())
	if err != nil {
		return 0, fmt.Errorf("idempotency cleanup: %w", err)
	}
	return res.RowsAffected()
}

// requestHasher starts the request hash with method, path and query.
func requestHasher(r *http.Request) hash.Hash {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s?%s\n", r.Method, r.URL.Path, r.URL.RawQuery)
	return h
}

// hashingBody feeds every byte read from the request body into h. When
// fixed is set (a re-run of a known request) sum returns it instead.
//
// Close drains the unread rest of the body into the hash before it closes
// the underlying body: handlers (Huma's body reader) close the body when
// they are done with it, and a real server's body can't be read after
// that, so draining it later would fail.
type hashingBody struct {
	io.ReadCloser
	fixed string

	mu sync.Mutex
	h  hash.Hash
	n  int64

	drainOnce sync.Once
	drainErr  error
}

// drain reads the rest of the body (up to the tracking limit) into the
// hash, once; it returns the read error, if any.
func (b *hashingBody) drain() error {
	b.drainOnce.Do(func() {
		_, b.drainErr = io.Copy(io.Discard, io.LimitReader(readerFunc(b.Read), maxHashedRequestBodySize-b.bytesRead()+1))
	})
	return b.drainErr
}

// Close drains the body into the hash, then closes it.
func (b *hashingBody) Close() error {
	_ = b.drain()
	return b.ReadCloser.Close()
}

// bytesRead is the number of body bytes hashed so far.
func (b *hashingBody) bytesRead() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// readerFunc adapts a Read method to io.Reader (so io.Copy doesn't use
// the body's own WriterTo or ReaderFrom and bypass the hash).
type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func (b *hashingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	b.h.Write(p[:n])
	b.n += int64(n)
	b.mu.Unlock()
	return n, err
}

// sum returns the hex hash of the request read so far.
func (b *hashingBody) sum() string {
	if b.fixed != "" {
		return b.fixed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return hex.EncodeToString(b.h.Sum(nil))
}

// captureWriter passes the response through and keeps a copy of it.
type captureWriter struct {
	http.ResponseWriter
	status   int
	buf      bytes.Buffer
	overflow bool
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if !c.overflow {
		if c.buf.Len()+len(b) > maxStoredResponseBytes {
			c.overflow = true
			c.buf.Reset()
		} else {
			c.buf.Write(b)
		}
	}
	return c.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// Flush supports streaming responses.
func (c *captureWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
