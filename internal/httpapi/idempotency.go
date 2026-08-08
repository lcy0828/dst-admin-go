package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/authn"

	"github.com/gin-gonic/gin"
)

const (
	defaultIdempotencyTTL          = 15 * time.Minute
	defaultIdempotencyEntries      = 2048
	maxIdempotencyRequestBody      = 2 << 20
	maxIdempotencyResponseBody     = 2 << 20
	idempotencyReplayHeader        = "Idempotency-Replayed"
	idempotencyRequestHeader       = "Idempotency-Key"
	idempotencyRequiredErrorCode   = "IDEMPOTENCY_KEY_REQUIRED"
	idempotencyConflictErrorCode   = "IDEMPOTENCY_CONFLICT"
	idempotencyCapacityErrorCode   = "IDEMPOTENCY_CAPACITY"
	idempotencyInvalidKeyErrorCode = "IDEMPOTENCY_KEY_INVALID"
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)

type idempotencyEntry struct {
	fingerprint string
	createdAt   time.Time
	expiresAt   time.Time
	done        chan struct{}
	status      int
	header      http.Header
	body        []byte
	completed   bool
}

// IdempotencyStore coalesces authenticated write retries without persisting
// response bodies that can contain one-time credentials.
type IdempotencyStore struct {
	mu         sync.Mutex
	entries    map[string]*idempotencyEntry
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
}

func NewIdempotencyStore(ttl time.Duration, maxEntries int) *IdempotencyStore {
	if ttl <= 0 {
		ttl = defaultIdempotencyTTL
	}
	if maxEntries <= 0 {
		maxEntries = defaultIdempotencyEntries
	}
	return &IdempotencyStore{
		entries: make(map[string]*idempotencyEntry),
		ttl:     ttl, maxEntries: maxEntries, now: time.Now,
	}
}

func (s *IdempotencyStore) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isUnsafeMethod(c.Request.Method) {
			c.Next()
			return
		}

		adminValue, authenticated := c.Get(authn.ContextAdminKey)
		admin, ok := adminValue.(authn.Admin)
		if !authenticated || !ok {
			// Public setup and login requests are protected by their own single-admin
			// and rate-limit semantics, and have no stable authenticated scope.
			c.Next()
			return
		}
		key := strings.TrimSpace(c.GetHeader(idempotencyRequestHeader))
		if key == "" {
			Failure(c, http.StatusBadRequest, idempotencyRequiredErrorCode, "写请求必须提供 Idempotency-Key", nil)
			return
		}
		if !idempotencyKeyPattern.MatchString(key) {
			Failure(c, http.StatusBadRequest, idempotencyInvalidKeyErrorCode, "Idempotency-Key 格式无效", nil)
			return
		}

		fingerprint, err := requestFingerprint(c.Request)
		if err != nil {
			Failure(c, http.StatusBadRequest, "REQUEST_READ_FAILED", "无法读取请求内容", nil)
			return
		}
		scope := fmt.Sprintf("%d\x00%s\x00%s\x00%s", admin.ID, c.Request.Method, c.Request.URL.EscapedPath(), key)
		entry, owner, available := s.reserve(scope, fingerprint)
		if !available {
			Failure(c, http.StatusServiceUnavailable, idempotencyCapacityErrorCode, "幂等请求缓存暂时已满，请稍后重试", nil)
			return
		}
		if !owner {
			s.replay(c, scope, fingerprint, entry)
			return
		}

		writer := &idempotencyResponseWriter{ResponseWriter: c.Writer, limit: maxIdempotencyResponseBody}
		c.Writer = writer
		completed := false
		defer func() {
			if !completed {
				s.abandon(scope, entry)
			}
		}()
		c.Next()
		completed = true
		if writer.overflow {
			s.abandon(scope, entry)
			return
		}
		s.complete(scope, entry, writer.Status(), c.Writer.Header(), writer.body.Bytes())
	}
}

func (s *IdempotencyStore) reserve(scope, fingerprint string) (*idempotencyEntry, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.expireLocked(now)
	if existing, ok := s.entries[scope]; ok {
		return existing, false, true
	}
	if len(s.entries) >= s.maxEntries && !s.evictOldestCompletedLocked() {
		return nil, false, false
	}
	entry := &idempotencyEntry{fingerprint: fingerprint, createdAt: now, done: make(chan struct{})}
	s.entries[scope] = entry
	return entry, true, true
}

func (s *IdempotencyStore) complete(scope string, entry *idempotencyEntry, status int, header http.Header, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[scope] != entry || entry.completed {
		return
	}
	entry.status = status
	entry.header = replayHeaders(header)
	entry.body = append([]byte(nil), body...)
	entry.expiresAt = s.now().Add(s.ttl)
	entry.completed = true
	close(entry.done)
}

func (s *IdempotencyStore) abandon(scope string, entry *idempotencyEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[scope] != entry || entry.completed {
		return
	}
	delete(s.entries, scope)
	close(entry.done)
}

func (s *IdempotencyStore) replay(c *gin.Context, scope, fingerprint string, entry *idempotencyEntry) {
	if entry.fingerprint != fingerprint {
		Failure(c, http.StatusConflict, idempotencyConflictErrorCode, "同一 Idempotency-Key 不能用于不同的请求内容", nil)
		return
	}
	select {
	case <-entry.done:
	case <-c.Request.Context().Done():
		c.Abort()
		return
	}

	s.mu.Lock()
	current, exists := s.entries[scope]
	if !exists || current != entry || !entry.completed || !entry.expiresAt.After(s.now()) {
		s.mu.Unlock()
		Failure(c, http.StatusServiceUnavailable, "IDEMPOTENCY_RETRY", "原请求未能完成，请使用新的 Idempotency-Key 重试", nil)
		return
	}
	status := entry.status
	header := entry.header.Clone()
	body := append([]byte(nil), entry.body...)
	s.mu.Unlock()

	for name, values := range header {
		c.Writer.Header().Del(name)
		for _, value := range values {
			c.Writer.Header().Add(name, value)
		}
	}
	c.Header(idempotencyReplayHeader, "true")
	c.Status(status)
	_, _ = c.Writer.Write(body)
	c.Abort()
}

func (s *IdempotencyStore) expireLocked(now time.Time) {
	for scope, entry := range s.entries {
		if entry.completed && !entry.expiresAt.After(now) {
			delete(s.entries, scope)
		}
	}
}

func (s *IdempotencyStore) evictOldestCompletedLocked() bool {
	var oldestScope string
	var oldest *idempotencyEntry
	for scope, entry := range s.entries {
		if !entry.completed || (oldest != nil && !entry.createdAt.Before(oldest.createdAt)) {
			continue
		}
		oldestScope, oldest = scope, entry
	}
	if oldest == nil {
		return false
	}
	delete(s.entries, oldestScope)
	return true
}

func requestFingerprint(request *http.Request) (string, error) {
	hash := sha256.New()
	_, _ = io.WriteString(hash, request.Method+"\n"+request.URL.EscapedPath()+"\n"+request.URL.RawQuery+"\n")
	contentType := strings.TrimSpace(request.Header.Get("Content-Type"))
	_, _ = io.WriteString(hash, contentType+"\n"+strconv.FormatInt(request.ContentLength, 10)+"\n")
	if request.Body == nil || request.ContentLength < 0 || request.ContentLength > maxIdempotencyRequestBody || strings.HasPrefix(strings.ToLower(contentType), "multipart/") {
		return hex.EncodeToString(hash.Sum(nil)), nil
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxIdempotencyRequestBody+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxIdempotencyRequestBody {
		request.Body = &combinedReadCloser{Reader: io.MultiReader(bytes.NewReader(body), request.Body), Closer: request.Body}
		return hex.EncodeToString(hash.Sum(nil)), nil
	}
	request.Body = &combinedReadCloser{Reader: bytes.NewReader(body), Closer: request.Body}
	_, _ = hash.Write(body)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func replayHeaders(source http.Header) http.Header {
	header := source.Clone()
	for _, name := range []string{"Connection", "Content-Length", "Date", "Keep-Alive", "Set-Cookie", "Transfer-Encoding"} {
		header.Del(name)
	}
	return header
}

type combinedReadCloser struct {
	io.Reader
	io.Closer
}

type idempotencyResponseWriter struct {
	gin.ResponseWriter
	body     bytes.Buffer
	limit    int
	overflow bool
}

func (w *idempotencyResponseWriter) Write(data []byte) (int, error) {
	w.capture(data)
	return w.ResponseWriter.Write(data)
}

func (w *idempotencyResponseWriter) WriteString(value string) (int, error) {
	w.capture([]byte(value))
	return w.ResponseWriter.WriteString(value)
}

func (w *idempotencyResponseWriter) capture(data []byte) {
	if w.overflow {
		return
	}
	if w.body.Len()+len(data) > w.limit {
		w.overflow = true
		w.body.Reset()
		return
	}
	_, _ = w.body.Write(data)
}
