package cache

import (
	"errors"
	"strings"
	"testing"

	"github.com/carder-coder/ai-agent/gateway/internal/provider"
)

func TestLookupRequiresMatchingScopeAndConservativeSimilarity(t *testing.T) {
	store := New(0.95, 0)
	query := "请总结人工智能技术在教育领域的主要优势与实际风险"
	if !store.Put(Entry{
		ID:    "source",
		Scope: "system-a",
		Query: query,
		Body:  []byte(`{"choices":[]}`),
		Usage: provider.Usage{InputTokens: 100, TotalTokens: 100},
	}) {
		t.Fatal("Put() rejected a small response")
	}

	if _, _, ok := store.Lookup("system-b", query); ok {
		t.Fatal("Lookup() reused a response under a different system prompt")
	}
	if _, _, ok := store.Lookup("system-a", "请给出不同主题的回答"); ok {
		t.Fatal("Lookup() returned a semantically different answer")
	}
	if _, _, ok := store.Lookup("system-a", query); !ok {
		t.Fatal("Lookup() missed an exact query")
	}
}

func TestFeedbackRequiresKnownHitAndCanBeSubmittedOnlyOnce(t *testing.T) {
	store := New(0.95, 0)
	if err := store.Feedback("unknown"); !errors.Is(err, ErrFeedbackNotFound) {
		t.Fatalf("Feedback() error = %v, want not found", err)
	}
	if !store.Put(Entry{ID: "entry", Scope: "scope", Query: "exact", Body: []byte("{}")}) {
		t.Fatal("Put() rejected a small response")
	}
	if err := store.TrackHit("trace-1", "entry"); err != nil {
		t.Fatalf("TrackHit() error = %v", err)
	}
	if err := store.Feedback("trace-1"); err != nil {
		t.Fatalf("Feedback() error = %v", err)
	}
	if err := store.Feedback("trace-1"); !errors.Is(err, ErrFeedbackAlreadyHandled) {
		t.Fatalf("duplicate Feedback() error = %v, want duplicate", err)
	}
}

func TestLookupCopiesResponseBody(t *testing.T) {
	store := New(0.95, 0)
	if !store.Put(Entry{ID: "entry", Scope: "scope", Query: "query", Body: []byte("body")}) {
		t.Fatal("Put() rejected a small response")
	}
	entry, _, ok := store.Lookup("scope", "query")
	if !ok {
		t.Fatal("Lookup() missed an exact query")
	}
	entry.Body[0] = 'B'
	again, _, ok := store.Lookup("scope", "query")
	if !ok || string(again.Body) != "body" {
		t.Fatalf("Lookup() leaked mutable cache bytes: %q", again.Body)
	}
}

func TestLookupAcceptsOnlyVerySimilarLongQueries(t *testing.T) {
	var builder strings.Builder
	for i := 0; i < 200; i++ {
		builder.WriteRune(rune(0x4e00 + i))
	}
	query := builder.String()
	store := New(0.95, 0)
	if !store.Put(Entry{ID: "entry", Scope: "scope", Query: query, Body: []byte("{}")}) {
		t.Fatal("Put() rejected a small response")
	}
	if _, score, ok := store.Lookup("scope", query+"文"); !ok || score < 0.95 {
		t.Fatalf("Lookup() did not accept a conservative near match: score=%f, ok=%t", score, ok)
	}
}
