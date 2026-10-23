package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

func TestHealthStateInitiallyDisconnected(t *testing.T) {
	h := newHealthState()
	s := h.Snapshot()
	if s.Success || s.Connected || s.LoggedIn {
		t.Fatalf("expected disconnected initial state, got %+v", s)
	}
	if s.LastEventAt != "" {
		t.Fatalf("expected empty last_event_at before any event, got %q", s.LastEventAt)
	}
}

func TestHealthStateConnect(t *testing.T) {
	h := newHealthState()
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	h.handleEventAt(&events.Connected{}, now)

	s := h.Snapshot()
	if !s.Success || !s.Connected || !s.LoggedIn {
		t.Fatalf("expected connected state after Connected event, got %+v", s)
	}
	if s.LastError != "" {
		t.Fatalf("expected no last_error after connect, got %q", s.LastError)
	}
	if s.LastEventAt != "2026-10-21T08:00:00Z" {
		t.Fatalf("unexpected last_event_at %q", s.LastEventAt)
	}
}

func TestHealthStateDisconnect(t *testing.T) {
	h := newHealthState()
	h.handleEventAt(&events.Connected{}, time.Now())
	h.handleEventAt(&events.Disconnected{}, time.Now())

	s := h.Snapshot()
	if s.Success || s.Connected {
		t.Fatalf("expected disconnected state after Disconnected event, got %+v", s)
	}
	if !s.LoggedIn {
		t.Fatal("a socket drop must not clear logged_in; the session is still paired")
	}
	if s.LastError == "" {
		t.Fatal("expected last_error to describe the disconnect")
	}
}

func TestHealthStateClientOutdated405(t *testing.T) {
	h := newHealthState()
	h.handleEventAt(&events.Connected{}, time.Now())
	h.handleEventAt(&events.ClientOutdated{}, time.Now())

	s := h.Snapshot()
	if s.Success || s.Connected {
		t.Fatalf("expected disconnected state after ClientOutdated, got %+v", s)
	}
	if !strings.Contains(s.LastError, "405") {
		t.Fatalf("expected last_error to mention the 405, got %q", s.LastError)
	}
}

func TestHealthStateConnectFailure(t *testing.T) {
	h := newHealthState()
	h.handleEventAt(&events.ConnectFailure{
		Reason:  events.ConnectFailureClientOutdated,
		Message: "client is out of date",
	}, time.Now())

	s := h.Snapshot()
	if s.Connected {
		t.Fatalf("expected disconnected state after ConnectFailure, got %+v", s)
	}
	if !strings.Contains(s.LastError, "405") || !strings.Contains(s.LastError, "client is out of date") {
		t.Fatalf("expected last_error with reason code and message, got %q", s.LastError)
	}
}

func TestHealthStateStreamReplaced(t *testing.T) {
	h := newHealthState()
	h.handleEventAt(&events.Connected{}, time.Now())
	h.handleEventAt(&events.StreamReplaced{}, time.Now())

	s := h.Snapshot()
	if s.Connected {
		t.Fatalf("expected disconnected state after StreamReplaced, got %+v", s)
	}
	if !strings.Contains(s.LastError, "stream replaced") {
		t.Fatalf("expected stream replaced last_error, got %q", s.LastError)
	}
}

func TestHealthStateLoggedOut(t *testing.T) {
	h := newHealthState()
	h.handleEventAt(&events.Connected{}, time.Now())
	h.handleEventAt(&events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, time.Now())

	s := h.Snapshot()
	if s.Connected || s.LoggedIn {
		t.Fatalf("expected fully logged-out state, got %+v", s)
	}
	if s.LastError == "" {
		t.Fatal("expected last_error to describe the logout")
	}
}

func TestHealthStateReconnectClearsError(t *testing.T) {
	h := newHealthState()
	h.handleEventAt(&events.Connected{}, time.Now())
	h.handleEventAt(&events.ClientOutdated{}, time.Now())
	h.handleEventAt(&events.Connected{}, time.Now())

	s := h.Snapshot()
	if !s.Connected || !s.LoggedIn || s.LastError != "" {
		t.Fatalf("expected clean connected state after reconnect, got %+v", s)
	}
}

func TestHealthStateKeepAliveTimeoutKeepsConnected(t *testing.T) {
	h := newHealthState()
	h.handleEventAt(&events.Connected{}, time.Now())
	h.handleEventAt(&events.KeepAliveTimeout{ErrorCount: 2}, time.Now())

	s := h.Snapshot()
	if !s.Connected {
		t.Fatalf("keepalive timeout alone must not flip connected, got %+v", s)
	}
	if !strings.Contains(s.LastError, "keepalive") {
		t.Fatalf("expected keepalive last_error, got %q", s.LastError)
	}
}

func TestHealthStateLastEventAtTracksAnyEvent(t *testing.T) {
	h := newHealthState()
	first := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	second := first.Add(5 * time.Minute)
	h.handleEventAt(&events.Connected{}, first)
	h.handleEventAt(&events.Message{}, second)

	s := h.Snapshot()
	if s.LastEventAt != "2026-10-21T08:05:00Z" {
		t.Fatalf("expected last_event_at to advance with every event, got %q", s.LastEventAt)
	}
}

func doHealthRequest(t *testing.T, h *healthState, token, sentToken string) (*http.Response, healthSnapshot) {
	t.Helper()
	handler := healthHandler(h, token)
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	if sentToken != "" {
		request.Header.Set(bridgeTokenHeader, sentToken)
	}
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	response := recorder.Result()

	var body healthSnapshot
	if response.StatusCode != http.StatusUnauthorized {
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode health response: %v", err)
		}
	}
	return response, body
}

func TestHealthEndpointConnected(t *testing.T) {
	h := newHealthState()
	h.handleEventAt(&events.Connected{}, time.Now())

	response, body := doHealthRequest(t, h, "secret", "secret")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 while connected, got %d", response.StatusCode)
	}
	if !body.Success || !body.Connected {
		t.Fatalf("expected success+connected body, got %+v", body)
	}
}

func TestHealthEndpointDisconnectedReturns503(t *testing.T) {
	h := newHealthState()
	h.handleEventAt(&events.Connected{}, time.Now())
	h.handleEventAt(&events.ClientOutdated{}, time.Now())

	response, body := doHealthRequest(t, h, "secret", "secret")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while disconnected, got %d", response.StatusCode)
	}
	if body.Success || body.Connected {
		t.Fatalf("expected non-success body while disconnected, got %+v", body)
	}
	if !strings.Contains(body.LastError, "405") {
		t.Fatalf("expected the 405 in last_error, got %q", body.LastError)
	}
}

func TestHealthEndpointRejectsBadToken(t *testing.T) {
	h := newHealthState()
	response, _ := doHealthRequest(t, h, "secret", "wrong")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad token, got %d", response.StatusCode)
	}
}
