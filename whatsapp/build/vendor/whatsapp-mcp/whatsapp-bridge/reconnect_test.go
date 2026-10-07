package main

import (
	"testing"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestReconnectDelay(t *testing.T) {
	cases := []struct {
		streak int
		want   time.Duration
	}{
		{streak: 0, want: 0},
		{streak: 1, want: 2 * time.Second},
		{streak: 2, want: 4 * time.Second},
		{streak: 3, want: 8 * time.Second},
		{streak: 8, want: 256 * time.Second},
		{streak: 9, want: reconnectMaxDelay},
		{streak: 20, want: reconnectMaxDelay},
		{streak: 1000, want: reconnectMaxDelay},
		{streak: -1, want: 0},
	}
	for _, tc := range cases {
		if got := reconnectDelay(tc.streak); got != tc.want {
			t.Errorf("reconnectDelay(%d) = %v, want %v", tc.streak, got, tc.want)
		}
	}
}

func TestSupervisorStatusTracking(t *testing.T) {
	s := newConnectionSupervisor(nil, waLog.Noop)

	status := s.Status()
	if status.Reconnecting || status.LoggedOut || status.LastDisconnect != "" {
		t.Fatalf("unexpected initial status: %+v", status)
	}

	s.NoteDisconnect("stream replaced")
	status = s.Status()
	if status.LastDisconnect != "stream replaced" {
		t.Errorf("LastDisconnect = %q, want %q", status.LastDisconnect, "stream replaced")
	}
	if status.LastDisconnectAt.IsZero() {
		t.Error("LastDisconnectAt should be set after NoteDisconnect")
	}
	select {
	case reason := <-s.kick:
		if reason != "stream replaced" {
			t.Errorf("kick reason = %q, want %q", reason, "stream replaced")
		}
	default:
		t.Error("NoteDisconnect should queue a reconnect kick")
	}

	// A second NoteDisconnect with a full kick channel must not block.
	s.NoteDisconnect("first")
	s.NoteDisconnect("second")
	if got := s.Status().LastDisconnect; got != "second" {
		t.Errorf("LastDisconnect = %q, want %q", got, "second")
	}

	s.mu.Lock()
	s.reconnecting = true
	s.failureStreak = 5
	s.mu.Unlock()
	s.NoteLoggedOut()
	status = s.Status()
	if !status.LoggedOut {
		t.Error("LoggedOut should be true after NoteLoggedOut")
	}
	if status.Reconnecting {
		t.Error("Reconnecting should be cleared after NoteLoggedOut")
	}
	if !s.isLoggedOut() {
		t.Error("isLoggedOut should report true")
	}
}

func TestNoteConnectedResetsBackoff(t *testing.T) {
	s := newConnectionSupervisor(nil, waLog.Noop)
	s.mu.Lock()
	s.reconnecting = true
	s.failureStreak = 7
	s.mu.Unlock()

	s.NoteConnected()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reconnecting {
		t.Error("reconnecting should be false after NoteConnected")
	}
	if s.failureStreak != 0 {
		t.Errorf("failureStreak = %d, want 0", s.failureStreak)
	}
}
