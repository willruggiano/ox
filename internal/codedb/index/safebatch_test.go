package index

import (
	"errors"
	"strings"
	"testing"
)

// --- safeBatch ---

// TestSafeBatch_RecoversPanic verifies that a panic inside the batch closure
// is converted to a normal error return.
//
// Failure prevented: a nil-FST panic in Bleve.Batch escapes the call site,
// unwinds through doIndex's deferred db.Close(), and deadlocks the daemon on
// the bbolt mutex still held by the panicked batch (observed wedge:
// 39 minutes in sync.Mutex.Lock with indexing_now reporting true).
func TestSafeBatch_RecoversPanic(t *testing.T) {
	t.Parallel()
	err := safeBatch(func() error {
		panic("simulated vellum nil FST")
	})
	if err == nil {
		t.Fatal("expected error from panicking batch, got nil")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("error should mention panic, got: %v", err)
	}
	if !strings.Contains(err.Error(), "simulated vellum nil FST") {
		t.Errorf("error should preserve panic value, got: %v", err)
	}
}

// TestSafeBatch_PassesThroughError ensures normal (non-panic) errors from the
// batch closure flow through unchanged so callers' errors.Is/As checks keep
// working.
func TestSafeBatch_PassesThroughError(t *testing.T) {
	t.Parallel()
	want := errors.New("real batch error")
	got := safeBatch(func() error { return want })
	if !errors.Is(got, want) {
		t.Errorf("want %v, got %v", want, got)
	}
}

// TestSafeBatch_NilOnSuccess ensures the happy path returns nil with no
// allocation surprises from the recover defer.
func TestSafeBatch_NilOnSuccess(t *testing.T) {
	t.Parallel()
	if err := safeBatch(func() error { return nil }); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

// TestSafeBatch_RecoversNonStringPanic verifies the recover handles arbitrary
// panic values (e.g. runtime.Error from a nil pointer dereference), not just
// string panics.
func TestSafeBatch_RecoversNonStringPanic(t *testing.T) {
	t.Parallel()
	err := safeBatch(func() error {
		var p *int
		_ = *p // nil pointer deref → runtime.Error
		return nil
	})
	if err == nil {
		t.Fatal("expected error from nil-deref panic, got nil")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("error should mention panic, got: %v", err)
	}
}
