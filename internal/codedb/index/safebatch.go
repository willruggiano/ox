package index

import (
	"fmt"
	"runtime/debug"
)

// safeBatch runs fn (a bleve.Index.Batch call) with panic recovery.
//
// Why: a corrupt Bleve segment — observed in production as a nil FST left by
// partial-write disk corruption — panics deep inside vellum during batch
// processing. Without recovery the panic unwinds through the caller's
// deferred db.Close(), which contends for the same bbolt lock the batch was
// holding, producing a self-deadlock that pins the daemon (one incident:
// 39 minutes wedged in sync.Mutex.Lock while indexing_now stayed true).
//
// Recovering at the batch boundary converts the panic into a normal error
// return so the caller's Close path runs and the failure surfaces.
func safeBatch(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("bleve batch panicked (likely corrupt index): %v\n%s", r, debug.Stack())
		}
	}()
	return fn()
}
