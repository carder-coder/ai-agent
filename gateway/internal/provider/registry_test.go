package provider

import (
	"context"
	"strings"
	"testing"
)

type testAdapter struct{}

func (testAdapter) Chat(context.Context, []byte, bool) (Response, error) {
	return Response{StatusCode: 200}, nil
}

func TestRegistryRegistersAndLooksUpAdapter(t *testing.T) {
	registry := NewRegistry()
	adapter := testAdapter{}
	if err := registry.Register("test", adapter); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	got, err := registry.Get("test")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if _, ok := got.(testAdapter); !ok {
		t.Fatalf("Get() returned %T, want testAdapter", got)
	}
}

func TestRegistryRejectsInvalidRegistrations(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register("", testAdapter{}); err == nil {
		t.Fatal("Register() accepted an empty provider name")
	}
	if err := registry.Register("test", nil); err == nil {
		t.Fatal("Register() accepted a nil adapter")
	}
	if err := registry.Register("test", testAdapter{}); err != nil {
		t.Fatalf("first Register() error = %v", err)
	}
	if err := registry.Register("test", testAdapter{}); err == nil {
		t.Fatal("Register() accepted a duplicate provider")
	}
	if _, err := registry.Get("missing"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("Get() error = %v, want unknown-provider error", err)
	}
}
