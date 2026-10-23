package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

// healthState tracks the real WhatsApp connection state from whatsmeow events
// so /api/health can distinguish "bridge process alive" from "ingest alive".
//
// It is event-driven on purpose: whatsmeow holds its socket lock for the whole
// dial + handshake inside Connect(), so calling client.IsConnected() from the
// health handler could hang health checks mid-reconnect.
type healthState struct {
	mu          sync.Mutex
	connected   bool
	loggedIn    bool
	lastError   string
	lastEventAt time.Time
}

// healthSnapshot is the JSON body served by /api/health.
type healthSnapshot struct {
	Success     bool   `json:"success"`
	Connected   bool   `json:"connected"`
	LoggedIn    bool   `json:"logged_in"`
	LastError   string `json:"last_error,omitempty"`
	LastEventAt string `json:"last_event_at,omitempty"`
}

func newHealthState() *healthState {
	return &healthState{}
}

// HandleEvent updates the tracked state. Register it with
// client.AddEventHandler; whatsmeow supports multiple handlers, so this stays
// independent of the message-ingest handler (and of any reconnect supervisor).
func (h *healthState) HandleEvent(evt interface{}) {
	h.handleEventAt(evt, time.Now())
}

func (h *healthState) handleEventAt(evt interface{}, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastEventAt = now

	switch v := evt.(type) {
	case *events.Connected:
		h.connected = true
		h.loggedIn = true
		h.lastError = ""
	case *events.Disconnected:
		h.connected = false
		h.lastError = "websocket disconnected"
	case *events.StreamReplaced:
		h.connected = false
		h.lastError = "stream replaced: another client connected with the same session"
	case *events.ClientOutdated:
		h.connected = false
		h.lastError = "client outdated (405): rebuild the bridge against a newer whatsmeow (scripts/build-bridge.sh)"
	case *events.TemporaryBan:
		h.connected = false
		h.lastError = "temporary ban: " + v.String()
	case *events.ConnectFailure:
		h.connected = false
		h.lastError = fmt.Sprintf("connect failure (%s): %s", v.Reason.NumberString(), v.Message)
	case *events.StreamError:
		h.connected = false
		h.lastError = "stream error: " + v.Code
	case *events.LoggedOut:
		h.connected = false
		h.loggedIn = false
		h.lastError = fmt.Sprintf("logged out by WhatsApp (reason=%s); re-pair via scripts/start-bridge.sh", v.Reason.String())
	case *events.KeepAliveTimeout:
		// Socket may still recover; record the symptom without flipping state.
		h.lastError = fmt.Sprintf("keepalive timeout (%d consecutive errors)", v.ErrorCount)
	}
}

// Snapshot returns the current state for serialization.
func (h *healthState) Snapshot() healthSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	snapshot := healthSnapshot{
		Success:   h.connected,
		Connected: h.connected,
		LoggedIn:  h.loggedIn,
		LastError: h.lastError,
	}
	if !h.lastEventAt.IsZero() {
		snapshot.LastEventAt = h.lastEventAt.UTC().Format(time.RFC3339)
	}
	return snapshot
}

// healthHandler serves /api/health: HTTP 200 with success=true while the
// WhatsApp connection is up, HTTP 503 with success=false and a last_error
// otherwise. Watchdogs that only care about process liveness should treat any
// authorized JSON response (200 or 503) as "bridge process is running".
func healthHandler(health *healthState, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeBridgeRequest(w, r, token) {
			return
		}
		snapshot := health.Snapshot()
		w.Header().Set("Content-Type", "application/json")
		if !snapshot.Connected {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(snapshot)
	}
}
