// 本包提供有界、进程内的语义缓存及按 trace ID 关联的答案反馈。
package cache

import (
	"errors"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/carder-coder/ai-agent/gateway/internal/provider"
)

var (
	ErrFeedbackNotFound       = errors.New("semantic cache feedback target not found")
	ErrFeedbackAlreadyHandled = errors.New("semantic cache feedback already submitted")
)

const (
	maxEntries       = 500
	maxBodyBytes     = 256 << 10
	maxCacheBytes    = 32 << 20
	maxFeedbackItems = 10_000
	feedbackTTL      = 24 * time.Hour
	minFuzzyRunes    = 12
)

// Entry 保存可复用的上游响应及其 usage；query 仅驻留内存且受 TTL/容量限制。
type Entry struct {
	ID        string
	Scope     string
	Query     string
	Body      []byte
	Usage     provider.Usage
	CreatedAt time.Time
}

type feedbackRef struct {
	entryID   string
	expiresAt time.Time
}

// Store 管理语义缓存条目及等待反馈的命中记录。
type Store struct {
	mu        sync.Mutex
	threshold float64
	ttl       time.Duration
	entries   map[string]Entry
	bodyBytes int
	pending   map[string]feedbackRef
	handled   map[string]time.Time
}

// New 创建缓存；阈值必须在 (0, 1]，无效值使用保守默认值 0.95。
func New(threshold float64, ttl time.Duration) *Store {
	if threshold <= 0 || threshold > 1 || math.IsNaN(threshold) {
		threshold = 0.95
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &Store{
		threshold: threshold,
		ttl:       ttl,
		entries:   make(map[string]Entry),
		pending:   make(map[string]feedbackRef),
		handled:   make(map[string]time.Time),
	}
}

// Lookup 返回同一模型与上下文 scope 下达到阈值的最高相似度缓存项。
func (s *Store) Lookup(scope, query string) (Entry, float64, bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)

	var best Entry
	bestScore := 0.0
	for _, entry := range s.entries {
		if entry.Scope != scope {
			continue
		}
		score := querySimilarity(query, entry.Query)
		if score >= s.threshold &&
			(score > bestScore || (score == bestScore && entry.CreatedAt.After(best.CreatedAt))) {
			best = entry
			bestScore = score
		}
	}
	if bestScore == 0 {
		return Entry{}, 0, false
	}
	best.Body = append([]byte(nil), best.Body...)
	return best, bestScore, true
}

// Put 保存成功的非流式响应；超过单条或总容量上限时不缓存。
func (s *Store) Put(entry Entry) bool {
	if entry.ID == "" || entry.Scope == "" || entry.Query == "" ||
		len(entry.Body) == 0 || len(entry.Body) > maxBodyBytes {
		return false
	}
	now := time.Now()
	entry.CreatedAt = now
	entry.Body = append([]byte(nil), entry.Body...)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)
	if previous, exists := s.entries[entry.ID]; exists {
		s.bodyBytes -= len(previous.Body)
		delete(s.entries, entry.ID)
	}
	for len(s.entries) >= maxEntries || s.bodyBytes+len(entry.Body) > maxCacheBytes {
		if !s.evictOldestLocked() {
			return false
		}
	}
	s.entries[entry.ID] = entry
	s.bodyBytes += len(entry.Body)
	return true
}

// TrackHit 关联一次命中 trace 与缓存条目，供后续正误反馈使用。
func (s *Store) TrackHit(traceID, entryID string) error {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)
	if _, exists := s.entries[entryID]; !exists {
		return ErrFeedbackNotFound
	}
	if _, exists := s.pending[traceID]; exists {
		return ErrFeedbackAlreadyHandled
	}
	if _, exists := s.handled[traceID]; exists {
		return ErrFeedbackAlreadyHandled
	}
	if len(s.pending) >= maxFeedbackItems {
		return errors.New("semantic cache feedback capacity reached")
	}
	s.pending[traceID] = feedbackRef{entryID: entryID, expiresAt: now.Add(feedbackTTL)}
	return nil
}

// Feedback 只接受每次语义缓存命中的一次正误评价。
func (s *Store) Feedback(traceID string) error {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)
	if _, exists := s.handled[traceID]; exists {
		return ErrFeedbackAlreadyHandled
	}
	if _, exists := s.pending[traceID]; !exists {
		return ErrFeedbackNotFound
	}
	if len(s.handled) >= maxFeedbackItems {
		return errors.New("semantic cache feedback capacity reached")
	}
	delete(s.pending, traceID)
	s.handled[traceID] = now.Add(feedbackTTL)
	return nil
}

func (s *Store) purgeExpiredLocked(now time.Time) {
	for id, entry := range s.entries {
		if now.Sub(entry.CreatedAt) >= s.ttl {
			s.bodyBytes -= len(entry.Body)
			delete(s.entries, id)
		}
	}
	for traceID, ref := range s.pending {
		if !now.Before(ref.expiresAt) {
			delete(s.pending, traceID)
		}
	}
	for traceID, expiresAt := range s.handled {
		if !now.Before(expiresAt) {
			delete(s.handled, traceID)
		}
	}
}

func (s *Store) evictOldestLocked() bool {
	var oldestID string
	var oldestAt time.Time
	for id, entry := range s.entries {
		if oldestID == "" || entry.CreatedAt.Before(oldestAt) {
			oldestID = id
			oldestAt = entry.CreatedAt
		}
	}
	if oldestID == "" {
		return false
	}
	s.bodyBytes -= len(s.entries[oldestID].Body)
	delete(s.entries, oldestID)
	return true
}

func querySimilarity(left, right string) float64 {
	left = normalize(left)
	right = normalize(right)
	if left == "" || right == "" {
		return 0
	}
	if left == right {
		return 1
	}
	if utf8.RuneCountInString(left) < minFuzzyRunes || utf8.RuneCountInString(right) < minFuzzyRunes {
		return 0
	}
	leftGrams := bigrams(left)
	rightGrams := bigrams(right)
	intersection := 0
	for gram := range leftGrams {
		if _, exists := rightGrams[gram]; exists {
			intersection++
		}
	}
	union := len(leftGrams) + len(rightGrams) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func normalize(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

func bigrams(value string) map[string]struct{} {
	runes := []rune(value)
	grams := make(map[string]struct{}, len(runes)-1)
	for i := 0; i+1 < len(runes); i++ {
		grams[string(runes[i:i+2])] = struct{}{}
	}
	return grams
}
