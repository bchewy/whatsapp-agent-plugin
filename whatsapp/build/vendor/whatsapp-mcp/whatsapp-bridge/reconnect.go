package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	// How often the watchdog verifies the websocket is still up. This is a
	// backstop for any disconnect path that produces no event we react to.
	reconnectWatchdogInterval = 1 * time.Minute
	reconnectBaseDelay        = 2 * time.Second
	reconnectMaxDelay         = 5 * time.Minute
)

// connectionSupervisor restores the WhatsApp connection after disconnects
// that whatsmeow marks as "expected" and therefore never auto-reconnects
// from. In particular, a "replaced" stream error (events.StreamReplaced)
// calls expectDisconnect() inside whatsmeow, so the websocket EOF that
// follows is swallowed silently: the REST server keeps answering /api/health
// while message ingest is frozen until the process is restarted. The same
// no-reconnect path applies to ClientOutdated (405) and unknown
// ConnectFailure reasons.
//
// Reconnects reuse the existing device session in whatsapp.db. Nothing here
// deletes the store or triggers QR pairing; after events.LoggedOut (where
// whatsmeow itself deletes the session) the supervisor stops trying.
type connectionSupervisor struct {
	client *whatsmeow.Client
	logger waLog.Logger

	kick chan string

	mu               sync.Mutex
	loggedOut        bool
	reconnecting     bool
	failureStreak    int
	lastDisconnect   string
	lastDisconnectAt time.Time
}

type supervisorStatus struct {
	Reconnecting     bool
	LoggedOut        bool
	LastDisconnect   string
	LastDisconnectAt time.Time
}

func newConnectionSupervisor(client *whatsmeow.Client, logger waLog.Logger) *connectionSupervisor {
	return &connectionSupervisor{
		client: client,
		logger: logger,
		kick:   make(chan string, 1),
	}
}

// NoteDisconnect records why the connection dropped and requests an immediate
// reconnect attempt.
func (s *connectionSupervisor) NoteDisconnect(reason string) {
	s.mu.Lock()
	s.lastDisconnect = reason
	s.lastDisconnectAt = time.Now()
	s.mu.Unlock()
	select {
	case s.kick <- reason:
	default:
	}
}

// NoteConnected resets backoff state after a successful (re)authentication.
func (s *connectionSupervisor) NoteConnected() {
	s.mu.Lock()
	wasReconnecting := s.reconnecting
	s.reconnecting = false
	s.failureStreak = 0
	s.mu.Unlock()
	if wasReconnecting {
		s.logger.Infof("WhatsApp connection restored; message ingest resumed")
	}
}

// NoteLoggedOut permanently stops reconnect attempts: the session store was
// deleted by whatsmeow and QR pairing is required.
func (s *connectionSupervisor) NoteLoggedOut() {
	s.mu.Lock()
	s.loggedOut = true
	s.reconnecting = false
	s.mu.Unlock()
}

func (s *connectionSupervisor) Status() supervisorStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return supervisorStatus{
		Reconnecting:     s.reconnecting,
		LoggedOut:        s.loggedOut,
		LastDisconnect:   s.lastDisconnect,
		LastDisconnectAt: s.lastDisconnectAt,
	}
}

func (s *connectionSupervisor) isLoggedOut() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loggedOut
}

// Run processes reconnect requests and runs the periodic watchdog. It never
// returns; start it in a goroutine once the initial connection is up.
func (s *connectionSupervisor) Run() {
	ticker := time.NewTicker(reconnectWatchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case reason := <-s.kick:
			// Event-driven reconnects force a socket teardown: right after a
			// StreamReplaced stream error the old socket may not have hit EOF
			// yet, so IsConnected() can still briefly report true.
			s.attemptReconnect(reason, true)
		case <-ticker.C:
			if s.client.IsConnected() || s.isLoggedOut() {
				continue
			}
			s.attemptReconnect("watchdog detected dropped connection", false)
		}
	}
}

func (s *connectionSupervisor) attemptReconnect(reason string, force bool) {
	s.mu.Lock()
	if s.loggedOut {
		s.mu.Unlock()
		s.logger.Warnf("Not reconnecting after %s: device is logged out, QR pairing required", reason)
		return
	}
	streak := s.failureStreak
	s.failureStreak++
	s.reconnecting = true
	s.mu.Unlock()

	if delay := reconnectDelay(streak); delay > 0 {
		s.logger.Infof("Waiting %v before WhatsApp reconnect attempt %d", delay, streak+1)
		time.Sleep(delay)
	}
	if s.isLoggedOut() {
		return
	}
	if !force && s.client.IsConnected() {
		s.mu.Lock()
		s.reconnecting = false
		s.mu.Unlock()
		return
	}

	s.logger.Warnf("Reconnecting to WhatsApp (%s); reusing existing session, no QR required", reason)
	// Tear down any half-dead socket so Connect() starts from a clean state
	// and clears whatsmeow's expected-disconnect flag.
	s.client.Disconnect()
	err := s.client.Connect()
	switch {
	case err == nil:
		s.logger.Infof("WhatsApp socket reconnected; waiting for authentication (Connected event)")
	case errors.Is(err, whatsmeow.ErrAlreadyConnected):
		s.logger.Infof("WhatsApp connection already re-established")
	default:
		s.logger.Errorf("WhatsApp reconnect attempt %d failed: %v", streak+1, err)
		s.NoteDisconnect(fmt.Sprintf("reconnect failed: %v", err))
	}
}

// reconnectDelay returns how long to wait before the given consecutive
// reconnect attempt: no delay for the first, then exponential backoff capped
// at reconnectMaxDelay. The streak only resets once authentication succeeds
// (events.Connected), so repeated server rejections such as ClientOutdated
// (405) cannot turn into a reconnect storm.
func reconnectDelay(streak int) time.Duration {
	if streak <= 0 {
		return 0
	}
	shift := streak - 1
	if shift > 10 {
		return reconnectMaxDelay
	}
	delay := reconnectBaseDelay << shift
	if delay > reconnectMaxDelay {
		return reconnectMaxDelay
	}
	return delay
}
