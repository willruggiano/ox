package main

import (
	"testing"

	"github.com/sageox/ox/internal/session"
	"github.com/stretchr/testify/assert"
)

func TestShouldPrune(t *testing.T) {
	tests := []struct {
		name     string
		status   session.SessionStatus
		pruneAll bool
		want     bool
	}{
		// default mode — only StatusLocal is pruned
		{"default_local", session.StatusLocal, false, true},
		{"default_paused", session.StatusPaused, false, false},
		{"default_canceled", session.StatusCanceled, false, false},
		{"default_ghost", session.StatusGhost, false, false},
		{"default_orphan", session.StatusOrphan, false, false},
		{"default_uploaded", session.StatusUploaded, false, false},
		{"default_recording", session.StatusRecording, false, false},

		// --all mode — every non-uploaded, non-recording status is pruned
		{"all_local", session.StatusLocal, true, true},
		{"all_paused", session.StatusPaused, true, true},
		{"all_canceled", session.StatusCanceled, true, true},
		{"all_ghost", session.StatusGhost, true, true},
		{"all_orphan", session.StatusOrphan, true, true},
		{"all_uploaded", session.StatusUploaded, true, false},
		{"all_recording", session.StatusRecording, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldPrune(tt.status, tt.pruneAll)
			assert.Equal(t, tt.want, got)
		})
	}
}
