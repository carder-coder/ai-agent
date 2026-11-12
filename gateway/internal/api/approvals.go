// 本文件实现短时、单次使用且绑定调用人与参数的写操作审批状态。
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/trace"
)

const (
	approvalTTL         = 5 * time.Minute
	maxPendingApprovals = 1000
)

var (
	errApprovalNotFound     = errors.New("approval not found")
	errApprovalExpired      = errors.New("approval expired")
	errApprovalNotConfirmed = errors.New("approval not confirmed")
	errApprovalMismatch     = errors.New("approval does not match request")
)

type approvalStore struct {
	mu      sync.Mutex
	entries map[string]approvalEntry
}

type approvalEntry struct {
	callerID  string
	tool      string
	digest    string
	expiresAt time.Time
	confirmed bool
}

func newApprovalStore() *approvalStore {
	return &approvalStore{entries: make(map[string]approvalEntry)}
}

func (s *approvalStore) Create(callerID, tool string, arguments map[string]any) (string, time.Time, error) {
	digest, err := argumentsDigest(arguments)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("hash approval arguments: %w", err)
	}
	id, err := trace.NewID()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate approval ID: %w", err)
	}
	now := time.Now()
	expiresAt := now.Add(approvalTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)
	if len(s.entries) >= maxPendingApprovals {
		return "", time.Time{}, errors.New("approval capacity reached")
	}
	s.entries[id] = approvalEntry{
		callerID:  callerID,
		tool:      tool,
		digest:    digest,
		expiresAt: expiresAt,
	}
	return id, expiresAt, nil
}

func (s *approvalStore) Confirm(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[id]
	if !exists {
		return errApprovalNotFound
	}
	if !time.Now().Before(entry.expiresAt) {
		delete(s.entries, id)
		return errApprovalExpired
	}
	entry.confirmed = true
	s.entries[id] = entry
	return nil
}

func (s *approvalStore) Consume(id, callerID, tool string, arguments map[string]any) error {
	digest, err := argumentsDigest(arguments)
	if err != nil {
		return fmt.Errorf("hash execution arguments: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[id]
	if !exists {
		return errApprovalNotFound
	}
	if !time.Now().Before(entry.expiresAt) {
		delete(s.entries, id)
		return errApprovalExpired
	}
	if !entry.confirmed {
		return errApprovalNotConfirmed
	}
	if entry.callerID != callerID || entry.tool != tool || entry.digest != digest {
		return errApprovalMismatch
	}
	delete(s.entries, id)
	return nil
}

func (s *approvalStore) purgeExpiredLocked(now time.Time) {
	for id, entry := range s.entries {
		if !now.Before(entry.expiresAt) {
			delete(s.entries, id)
		}
	}
}

func argumentsDigest(arguments map[string]any) (string, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validApprovalID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, value := range id {
		if !((value >= '0' && value <= '9') || (value >= 'a' && value <= 'f')) {
			return false
		}
	}
	return true
}

func writeApprovalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errApprovalNotFound):
		writeError(w, http.StatusNotFound, "approval is not found", "invalid_request_error", "approval_not_found")
	case errors.Is(err, errApprovalExpired):
		writeError(w, http.StatusConflict, "approval has expired", "invalid_request_error", "approval_expired")
	case errors.Is(err, errApprovalNotConfirmed):
		writeError(w, http.StatusConflict, "approval has not been confirmed", "invalid_request_error", "approval_not_confirmed")
	case errors.Is(err, errApprovalMismatch):
		writeError(w, http.StatusConflict, "approval does not match caller, tool, or arguments", "invalid_request_error", "approval_mismatch")
	default:
		writeError(w, http.StatusServiceUnavailable, "approval service is unavailable", "server_error", "approval_unavailable")
	}
}
