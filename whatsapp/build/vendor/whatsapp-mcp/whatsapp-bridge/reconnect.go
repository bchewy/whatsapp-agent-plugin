package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	// How often the watchdog verifies the websocket is still up. This is a
	// backstop for any disconnect path that produces no event we react to.
	reconnectWatchdogInterval = 1 * time.Minute
	reconnectBaseDelay        = 2 * time.Second
	reconnectMaxDelay         = 5 * time.Minute
	// A connection must stay authenticated this long before the backoff
	// streak resets. Without this, two bridges sharing one session would
	// replace each other (StreamReplaced -> reconnect -> Connected) in a
	// tight loop, since every round trip reaches Connected.
	reconnectStableAfter = 2 * time.Minute
	// How long Stop waits for an in-flight Connect() to return.
	supervisorStopTimeout = 5 * time.Second
)

// whatsappConn is the subset of *whatsmeow.Client the supervisor drives.
type whatsappConn interface {
	IsConnected() bool
	IsLoggedIn() bool
	Connect() error
	Disconnect()
}

// connectionSupervisor restores the WhatsApp connection after disconnects
// that whatsmeow marks as "expected" and therefore never auto-reconnects
// from. In particular, a "replaced" stream error (events.StreamReplaced)
// calls expectDisconnect() inside whatsmeow, so the websocket EOF that
// follows is swallowed silently: the REST server keeps answering /api/health
// while message ingest is frozen until the process is restarted. The same
// no-reconnect path applies to ClientOutdated (405), TemporaryBan and unknown
// ConnectFailure reasons.
//
// Reconnects reuse the existing device session in whatsapp.db. Nothing here
// deletes the store or triggers QR pairing; after events.LoggedOut (where
// whatsmeow itself deletes the session) the supervisor stops trying.
type connectionSupervisor struct {
	client whatsappConn
	logger waLog.Logger
	now    func() time.Time

	kick     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	mu               sync.Mutex
	loggedOut        bool
	stopped          bool
	reconnecting     bool
	failureStreak    int
	lastDisconnect   string
	lastDisconnectAt time.Time
	lastConnectedAt  time.Time
	retryNotBefore   time.Time
}

type supervisorStatus struct {
	Reconnecting     bool
	LoggedOut        bool
	LastDisconnect   string
	LastDisconnectAt time.Time
}

func newConnectionSupervisor(client whatsappConn, logger waLog.Logger) *connectionSupervisor {
	return &connectionSupervisor{
		client: client,
		logger: logger,
		now:    time.Now,
		kick:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// NoteDisconnect records why the connection dropped and requests a reconnect
// attempt.
func (s *connectionSupervisor) NoteDisconnect(reason string) {
	s.NoteDisconnectUntil(reason, time.Time{})
}

// NoteDisconnectUntil is NoteDisconnect with a floor on when the next attempt
// may run (e.g. a temporary ban's expiry).
func (s *connectionSupervisor) NoteDisconnectUntil(reason string, notBefore time.Time) {
	s.recordDisconnect(reason)
	s.mu.Lock()
	if notBefore.After(s.retryNotBefore) {
		s.retryNotBefore = notBefore
	}
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// NoteAutoReconnecting records a disconnect that whatsmeow recovers from on
// its own (events.Disconnected), so /api/health reflects it, without
// requesting an extra reconnect.
func (s *connectionSupervisor) NoteAutoReconnecting(reason string) {
	s.recordDisconnect(reason)
	s.mu.Lock()
	if !s.loggedOut {
		s.reconnecting = true
	}
	s.mu.Unlock()
}

func (s *connectionSupervisor) recordDisconnect(reason string) {
	s.mu.Lock()
	s.lastDisconnect = reason
	s.lastDisconnectAt = s.now()
	s.mu.Unlock()
}

// NoteConnected records a successful (re)authentication.
func (s *connectionSupervisor) NoteConnected() {
	s.mu.Lock()
	wasReconnecting := s.reconnecting
	s.reconnecting = false
	s.lastConnectedAt = s.now()
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

// shouldGiveUp reports whether reconnecting is pointless (logged out) or
// unwanted (shutting down).
func (s *connectionSupervisor) shouldGiveUp() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loggedOut || s.stopped
}

// Run processes reconnect requests and runs the periodic watchdog until Stop
// is called. Start it in a goroutine once the initial connection is up.
func (s *connectionSupervisor) Run() {
	s.run(reconnectWatchdogInterval)
}

func (s *connectionSupervisor) run(watchdogInterval time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.kick:
			// Event-driven reconnects force a socket teardown: right after a
			// StreamReplaced stream error the old socket may not have hit EOF
			// yet, so IsConnected() can still briefly report true.
			s.attemptReconnect(true)
		case <-ticker.C:
			if s.client.IsConnected() || s.isLoggedOut() {
				continue
			}
			s.recordDisconnect("watchdog detected dropped connection")
			s.attemptReconnect(false)
		}
	}
}

// Stop ends Run and prevents further reconnects. Call it before the final
// client.Disconnect() so the supervisor cannot reconnect during shutdown.
func (s *connectionSupervisor) Stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		close(s.stop)
	})
	select {
	case <-s.done:
	case <-time.After(supervisorStopTimeout):
		s.logger.Warnf("Connection supervisor did not stop within %v", supervisorStopTimeout)
	}
}

// restoredSinceLastDisconnect reports whether a Connected event arrived after
// the most recent recorded disconnect, i.e. the connection was already
// re-established (by a previous attempt or by whatsmeow) and a forced
// teardown would only drop a healthy session.
func (s *connectionSupervisor) restoredSinceLastDisconnect() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastConnectedAt.After(s.lastDisconnectAt)
}

// sleep waits for d or until Stop; it returns false if stopped.
func (s *connectionSupervisor) sleep(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.stop:
		return false
	}
}

// planAttempt advances the backoff streak and returns the 1-based attempt
// number and how long to wait before it. ok is false if no attempt should be
// made at all.
func (s *connectionSupervisor) planAttempt() (attempt int, delay time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loggedOut || s.stopped {
		if s.loggedOut {
			s.logger.Warnf("Not reconnecting: device is logged out, QR pairing required")
		}
		return 0, 0, false
	}
	now := s.now()
	if !s.lastConnectedAt.IsZero() && now.Sub(s.lastConnectedAt) >= reconnectStableAfter {
		s.failureStreak = 0
	}
	streak := s.failureStreak
	s.failureStreak++
	s.reconnecting = true
	delay = reconnectDelay(streak)
	if wait := s.retryNotBefore.Sub(now); wait > delay {
		delay = wait
	}
	return streak + 1, delay, true
}

func (s *connectionSupervisor) attemptReconnect(force bool) {
	attempt, delay, ok := s.planAttempt()
	if !ok {
		return
	}
	if delay > 0 {
		s.logger.Infof("Waiting %v before WhatsApp reconnect attempt %d", delay.Round(time.Second), attempt)
		if !s.sleep(delay) {
			return
		}
	}
	if s.shouldGiveUp() {
		return
	}
	if (force && s.restoredSinceLastDisconnect()) || (!force && s.client.IsConnected()) {
		s.mu.Lock()
		s.reconnecting = false
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	reason := s.lastDisconnect
	s.mu.Unlock()
	s.logger.Warnf("Reconnecting to WhatsApp (%s); reusing existing session, no QR required", reason)
	// Tear down any half-dead socket so Connect() starts from a clean state
	// and clears whatsmeow's expected-disconnect flag.
	s.client.Disconnect()
	if s.shouldGiveUp() {
		return
	}
	err := s.client.Connect()
	switch {
	case err == nil:
		s.logger.Infof("WhatsApp socket reconnected; waiting for authentication (Connected event)")
	case errors.Is(err, whatsmeow.ErrAlreadyConnected):
		s.logger.Infof("WhatsApp connection already re-established")
		s.mu.Lock()
		s.reconnecting = false
		s.mu.Unlock()
	case errors.Is(err, store.ErrDeviceDeleted):
		s.logger.Errorf("WhatsApp device session was deleted; stopping reconnects. Run scripts/start-bridge.sh to scan a new QR code")
		s.NoteLoggedOut()
	default:
		s.logger.Errorf("WhatsApp reconnect attempt %d failed: %v", attempt, err)
		s.NoteDisconnect(fmt.Sprintf("reconnect failed: %v", err))
	}
}

// reconnectDelay returns how long to wait before the given consecutive
// reconnect attempt: no delay for the first, then exponential backoff capped
// at reconnectMaxDelay. The streak only resets once a connection has stayed
// authenticated for reconnectStableAfter, so repeated server rejections such
// as ClientOutdated (405) or two bridges replacing each other cannot turn
// into a reconnect storm.
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

// probeConnected reports c.IsConnected() without blocking the caller longer
// than timeout. whatsmeow holds its socket lock for the whole dial and noise
// handshake inside Connect(), and ensure-bridge.sh gives /api/health 0.5s
// before deciding the bridge is down and launching a second one (which would
// itself trigger StreamReplaced). ok is false if the probe timed out.
func probeConnected(c whatsappConn, timeout time.Duration) (connected, ok bool) {
	ch := make(chan bool, 1)
	go func() { ch <- c.IsConnected() }()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case v := <-ch:
		return v, true
	case <-t.C:
		return false, false
	}
}
