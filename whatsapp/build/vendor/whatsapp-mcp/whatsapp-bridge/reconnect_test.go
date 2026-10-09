package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
)

type fakeConn struct {
	mu         sync.Mutex
	connected  bool
	loggedIn   bool
	connectErr error
	calls      []string
	block      chan struct{}
}

func (f *fakeConn) IsConnected() bool {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeConn) IsLoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loggedIn
}

func (f *fakeConn) Connect() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "connect")
	if f.connectErr != nil {
		return f.connectErr
	}
	f.connected = true
	return nil
}

func (f *fakeConn) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "disconnect")
	f.connected = false
}

func (f *fakeConn) callLog() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestSupervisor(conn whatsappConn) (*connectionSupervisor, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s := newConnectionSupervisor(conn, waLog.Noop)
	s.now = clock.now
	return s, clock
}

func drainKick(s *connectionSupervisor) bool {
	select {
	case <-s.kick:
		return true
	default:
		return false
	}
}

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
	s, _ := newTestSupervisor(&fakeConn{})

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
	if !drainKick(s) {
		t.Error("NoteDisconnect should queue a reconnect kick")
	}

	// A second NoteDisconnect with a full kick channel must not block.
	s.NoteDisconnect("first")
	s.NoteDisconnect("second")
	if got := s.Status().LastDisconnect; got != "second" {
		t.Errorf("LastDisconnect = %q, want %q", got, "second")
	}

	s.NoteAutoReconnecting("connection lost")
	if st := s.Status(); !st.Reconnecting || st.LastDisconnect != "connection lost" {
		t.Errorf("after NoteAutoReconnecting: %+v", st)
	}

	s.NoteLoggedOut()
	status = s.Status()
	if !status.LoggedOut {
		t.Error("LoggedOut should be true after NoteLoggedOut")
	}
	if status.Reconnecting {
		t.Error("Reconnecting should be cleared after NoteLoggedOut")
	}
	s.NoteAutoReconnecting("connection lost")
	if s.Status().Reconnecting {
		t.Error("Reconnecting must stay false once logged out")
	}
}

func TestForcedReconnectDisconnectsThenConnects(t *testing.T) {
	conn := &fakeConn{connected: true}
	s, _ := newTestSupervisor(conn)
	s.NoteDisconnect("stream replaced")
	drainKick(s)

	s.attemptReconnect(true)

	if got := conn.callLog(); got != "disconnect,connect" {
		t.Errorf("calls = %q, want disconnect,connect", got)
	}
	if !s.Status().Reconnecting {
		t.Error("should report reconnecting until the Connected event arrives")
	}
	s.NoteConnected()
	if s.Status().Reconnecting {
		t.Error("Connected should clear reconnecting")
	}
}

func TestShortLivedConnectionsKeepBackingOff(t *testing.T) {
	s, clock := newTestSupervisor(&fakeConn{})
	// Two bridges on one session: each reconnect authenticates, then gets
	// replaced a second later. The delay must keep growing.
	var delays []time.Duration
	for i := 0; i < 4; i++ {
		s.NoteConnected()
		clock.advance(time.Second)
		s.NoteDisconnect("stream replaced")
		_, delay, ok := s.planAttempt()
		if !ok {
			t.Fatal("planAttempt refused")
		}
		delays = append(delays, delay)
	}
	want := []time.Duration{0, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delays = %v, want %v", delays, want)
			break
		}
	}
}

func TestStableConnectionResetsBackoff(t *testing.T) {
	s, clock := newTestSupervisor(&fakeConn{})
	s.mu.Lock()
	s.failureStreak = 7
	s.mu.Unlock()
	s.NoteConnected()
	clock.advance(reconnectStableAfter)
	s.NoteDisconnect("stream replaced")

	attempt, delay, ok := s.planAttempt()
	if !ok || attempt != 1 || delay != 0 {
		t.Errorf("planAttempt = (%d, %v, %t), want (1, 0s, true)", attempt, delay, ok)
	}
}

func TestTemporaryBanWaitsForExpiry(t *testing.T) {
	s, clock := newTestSupervisor(&fakeConn{})
	s.NoteDisconnectUntil("temporary ban (code=101)", clock.now().Add(time.Hour))

	_, delay, ok := s.planAttempt()
	if !ok || delay != time.Hour {
		t.Errorf("planAttempt delay = %v (ok=%t), want 1h", delay, ok)
	}

	clock.advance(2 * time.Hour)
	_, delay, _ = s.planAttempt()
	if delay != reconnectDelay(1) {
		t.Errorf("after ban expiry delay = %v, want normal backoff %v", delay, reconnectDelay(1))
	}
}

func TestForcedReconnectSkippedWhenAlreadyRestored(t *testing.T) {
	conn := &fakeConn{connected: true, loggedIn: true}
	s, clock := newTestSupervisor(conn)
	s.NoteDisconnect("stream replaced")
	clock.advance(time.Second)
	// A previous attempt already brought the session back before this
	// queued kick was processed.
	s.NoteConnected()

	s.attemptReconnect(true)

	if got := conn.callLog(); got != "" {
		t.Errorf("healthy connection was torn down: calls = %q", got)
	}
	if s.Status().Reconnecting {
		t.Error("reconnecting should be false when the connection is already restored")
	}
}

func TestWatchdogSkipsWhenConnected(t *testing.T) {
	conn := &fakeConn{connected: true}
	s, _ := newTestSupervisor(conn)
	s.attemptReconnect(false)
	if got := conn.callLog(); got != "" {
		t.Errorf("calls = %q, want none", got)
	}
}

func TestReconnectErrorRequeues(t *testing.T) {
	conn := &fakeConn{connectErr: errors.New("dial tcp: i/o timeout")}
	s, _ := newTestSupervisor(conn)
	s.NoteDisconnect("stream replaced")
	drainKick(s)

	s.attemptReconnect(true)

	if !drainKick(s) {
		t.Error("failed Connect should queue another attempt")
	}
	if got := s.Status().LastDisconnect; !strings.Contains(got, "i/o timeout") {
		t.Errorf("LastDisconnect = %q, want the connect error", got)
	}
}

func TestAlreadyConnectedClearsReconnecting(t *testing.T) {
	conn := &fakeConn{connectErr: whatsmeow.ErrAlreadyConnected}
	s, _ := newTestSupervisor(conn)
	s.NoteDisconnect("stream replaced")
	drainKick(s)

	s.attemptReconnect(true)

	if s.Status().Reconnecting {
		t.Error("reconnecting should be cleared on ErrAlreadyConnected")
	}
	if drainKick(s) {
		t.Error("ErrAlreadyConnected must not queue another attempt")
	}
}

func TestDeletedDeviceStopsReconnecting(t *testing.T) {
	conn := &fakeConn{connectErr: store.ErrDeviceDeleted}
	s, _ := newTestSupervisor(conn)
	s.NoteDisconnect("stream replaced")
	drainKick(s)

	s.attemptReconnect(true)

	if !s.Status().LoggedOut {
		t.Error("ErrDeviceDeleted should mark the supervisor logged out")
	}
	if drainKick(s) {
		t.Error("ErrDeviceDeleted must not queue another attempt")
	}
	s.attemptReconnect(true)
	if got := conn.callLog(); got != "disconnect,connect" {
		t.Errorf("calls = %q, want a single attempt", got)
	}
}

func TestLoggedOutSkipsReconnect(t *testing.T) {
	conn := &fakeConn{}
	s, _ := newTestSupervisor(conn)
	s.NoteLoggedOut()
	s.attemptReconnect(true)
	s.attemptReconnect(false)
	if got := conn.callLog(); got != "" {
		t.Errorf("calls = %q, want none", got)
	}
}

func TestRunHandlesKicksAndStops(t *testing.T) {
	conn := &fakeConn{connected: true}
	s, _ := newTestSupervisor(conn)
	go s.run(time.Hour)

	s.NoteDisconnect("stream replaced")
	deadline := time.Now().Add(2 * time.Second)
	for conn.callLog() != "disconnect,connect" {
		if time.Now().After(deadline) {
			t.Fatalf("kick not handled, calls = %q", conn.callLog())
		}
		time.Sleep(5 * time.Millisecond)
	}

	s.Stop()
	select {
	case <-s.done:
	default:
		t.Fatal("Run did not exit after Stop")
	}
	s.Stop()
}

func TestRunWatchdogReconnectsDroppedSocket(t *testing.T) {
	conn := &fakeConn{}
	s, _ := newTestSupervisor(conn)
	go s.run(10 * time.Millisecond)
	defer s.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(conn.callLog(), "connect") {
		if time.Now().After(deadline) {
			t.Fatal("watchdog never reconnected")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.Status().LastDisconnect; got != "watchdog detected dropped connection" {
		t.Errorf("LastDisconnect = %q", got)
	}
}

func TestStopInterruptsBackoff(t *testing.T) {
	conn := &fakeConn{}
	s, _ := newTestSupervisor(conn)
	s.mu.Lock()
	s.failureStreak = 20
	s.mu.Unlock()
	go s.run(time.Hour)

	s.NoteDisconnect("client outdated (405)")
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	s.Stop()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Stop took %v while Run was backing off", elapsed)
	}
	if got := conn.callLog(); got != "" {
		t.Errorf("reconnected during shutdown: calls = %q", got)
	}
}

func TestConcurrentKicksDoNotDeadlock(t *testing.T) {
	conn := &fakeConn{connected: true}
	s, _ := newTestSupervisor(conn)
	go s.run(time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.NoteDisconnect("stream replaced")
			s.NoteConnected()
			_ = s.Status()
		}()
	}
	wg.Wait()

	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(supervisorStopTimeout + time.Second):
		t.Fatal("Stop hung after concurrent kicks")
	}
}

func TestProbeConnectedDoesNotBlock(t *testing.T) {
	conn := &fakeConn{connected: true}
	if connected, ok := probeConnected(conn, time.Second); !connected || !ok {
		t.Errorf("probeConnected = (%t, %t), want (true, true)", connected, ok)
	}

	conn.block = make(chan struct{})
	start := time.Now()
	connected, ok := probeConnected(conn, 20*time.Millisecond)
	if connected || ok {
		t.Errorf("probeConnected on a held lock = (%t, %t), want (false, false)", connected, ok)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("probeConnected blocked for %v", elapsed)
	}
	close(conn.block)
}
