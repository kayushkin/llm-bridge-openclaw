package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// newTestHarness builds a Harness with a live root context, the way NewHarness
// does, so a test can tell an interrupted turn from a shut-down session.
func newTestHarness(cfg *Config) *Harness {
	ctx, cancel := context.WithCancel(context.Background())
	return &Harness{cfg: cfg, ctx: ctx, cancel: cancel, bridgeSessionID: "bs_1"}
}

// TestWatchSignalsInterruptsTurnAndStaysAlive is the whole point of this
// change. SIGINT is llm-bridge-server's Stop, and the handler used to call
// os.Exit on it, so Stop killed the openclaw session instead of the turn.
//
// The two assertions are inseparable: cancelling the turn while also exiting
// would still lose the session, and staying alive without cancelling would
// make Stop do nothing at all.
func TestWatchSignalsInterruptsTurnAndStaysAlive(t *testing.T) {
	_, restore := captureEvents(t)
	defer restore()

	h := newTestHarness(&Config{})
	cancelled := make(chan struct{}, 1)
	h.turnCancel = func() { cancelled <- struct{}{} }

	terminated := make(chan struct{}, 1)
	sigs := make(chan os.Signal, 1)
	go watchSignals(h, sigs, func() { terminated <- struct{}{} })

	sigs <- syscall.SIGINT

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGINT did not cancel the in-flight turn")
	}

	select {
	case <-terminated:
		t.Fatal("SIGINT terminated the process — Stop killed the session, not the turn")
	case <-time.After(100 * time.Millisecond):
	}

	if h.ctx.Err() != nil {
		t.Errorf("harness root context is cancelled after an interrupt (%v) — the session did not survive", h.ctx.Err())
	}
}

// TestWatchSignalsHonoursEverySIGINT pins the range loop.
//
// A single `sig := <-sigs` read passes the test above and still leaves the
// session broken: the turn cancel is rebuilt per turn, so a handler that
// returns after one signal honours the first Stop of a session and silently
// ignores every Stop after it. That is the defect aider, forgecode and nanoclaw
// each carried, and it only shows up from the second Stop onwards.
func TestWatchSignalsHonoursEverySIGINT(t *testing.T) {
	_, restore := captureEvents(t)
	defer restore()

	h := newTestHarness(&Config{})
	cancelled := make(chan struct{}, 3)

	sigs := make(chan os.Signal, 1)
	go watchSignals(h, sigs, func() { t.Error("SIGINT terminated the process") })

	for turn := 1; turn <= 3; turn++ {
		// Each turn installs its own cancel, exactly as sendMessage does.
		h.turnMu.Lock()
		h.turnCancel = func() { cancelled <- struct{}{} }
		h.turnMu.Unlock()

		sigs <- syscall.SIGINT

		select {
		case <-cancelled:
		case <-time.After(2 * time.Second):
			t.Fatalf("turn %d: SIGINT was ignored — the handler stopped reading signals", turn)
		}
	}
}

// TestWatchSignalsShutsDownOnSIGTERM keeps the other half of the contract:
// SIGTERM still ends the session, so making SIGINT survivable did not make the
// process unkillable.
func TestWatchSignalsShutsDownOnSIGTERM(t *testing.T) {
	h := newTestHarness(&Config{})

	terminated := make(chan struct{}, 1)
	sigs := make(chan os.Signal, 1)
	go watchSignals(h, sigs, func() { terminated <- struct{}{} })

	sigs <- syscall.SIGTERM

	select {
	case <-terminated:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGTERM did not shut the harness down")
	}
	if h.ctx.Err() == nil {
		t.Error("SIGTERM left the harness root context live — Shutdown did not run")
	}
}

// TestHandleInterruptWithNoTurnSaysSo covers Stop pressed on an idle session.
// It must not crash on the nil cancel, and it must not report a cancellation
// that did not happen.
func TestHandleInterruptWithNoTurnSaysSo(t *testing.T) {
	events, restore := captureEvents(t)
	defer restore()

	h := newTestHarness(&Config{})
	if err := h.handleInterrupt(); err != nil {
		t.Fatalf("handleInterrupt on an idle session: %v", err)
	}

	got := events()
	if len(got) != 1 {
		t.Fatalf("emitted %d events, want exactly 1", len(got))
	}
	if got[0].System == nil || got[0].System.Subtype != "interrupt_noop" {
		t.Errorf("emitted %+v, want a system interrupt_noop event", got[0].System)
	}
}

// TestInterruptAbortsInFlightRequest is the live canary for the whole change,
// run against a real HTTP server rather than a stubbed cancel function.
//
// The server streams SSE forever and never sends [DONE], which is the shape of
// a long turn. Before this change sendMessage held the harness root context, so
// nothing short of killing the process could end that read — it ran to the
// 10-minute client timeout. The test asserts three things that only hold if the
// cancellation reaches the socket: the call returns, it returns
// context.Canceled rather than a completed turn, and the server observes the
// client disconnect.
func TestInterruptAbortsInFlightRequest(t *testing.T) {
	events, restore := captureEvents(t)
	defer restore()

	streaming := make(chan struct{})
	disconnected := make(chan struct{})
	// testDone releases the handler when the test ends. Without it a failing
	// run hangs instead of failing: the handler streams until the client
	// disconnects, and server.Close waits for outstanding handlers, so the
	// exact sabotage this test exists to catch — a turn that cannot be
	// cancelled — would deadlock the package rather than report itself.
	testDone := make(chan struct{})
	var closeStreaming, closeDisconnected bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server response writer cannot flush; the SSE stream would buffer")
			return
		}
		for {
			if _, err := w.Write([]byte("data: {\"choices\":[]}\n\n")); err != nil {
				break
			}
			flusher.Flush()
			if !closeStreaming {
				closeStreaming = true
				close(streaming)
			}
			select {
			case <-r.Context().Done():
				if !closeDisconnected {
					closeDisconnected = true
					close(disconnected)
				}
				return
			case <-testDone:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	// LIFO: close(testDone) runs first and lets the handler return, then
	// server.Close has nothing left to wait for.
	defer server.Close()
	defer close(testDone)

	h := newTestHarness(&Config{OpenClawURL: server.URL})
	h.agentID = "main"

	sendErr := make(chan error, 1)
	go func() { sendErr <- h.sendMessage("a long turn") }()

	// Wait for the turn to be genuinely in flight before interrupting it,
	// so a pass cannot come from cancelling a request that never started.
	select {
	case <-streaming:
	case <-time.After(5 * time.Second):
		t.Fatal("server never started streaming; the turn never went in flight")
	}

	if err := h.handleInterrupt(); err != nil {
		t.Fatalf("handleInterrupt: %v", err)
	}

	select {
	case err := <-sendErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sendMessage returned %v, want context.Canceled — an aborted turn was reported as a finished one", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sendMessage did not return after the interrupt; the turn ran on")
	}

	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the client go away; the cancellation did not reach the socket")
	}

	if h.ctx.Err() != nil {
		t.Errorf("interrupting a turn cancelled the harness root context (%v) — the tailer and session would die with it", h.ctx.Err())
	}

	var sawInterrupted, sawSendError bool
	for _, ev := range events() {
		if ev.Type == msg.EventError && ev.Error != nil {
			switch ev.Error.Code {
			case "INTERRUPTED":
				sawInterrupted = true
			case "SEND_ERROR":
				sawSendError = true
			}
		}
	}
	if !sawInterrupted {
		t.Error("no INTERRUPTED event — the transcript shows a turn that stops for no stated reason")
	}
	if sawSendError {
		t.Error("emitted SEND_ERROR for an interrupt — a UI cannot tell 'you stopped this' from 'this broke'")
	}
}

// TestTurnCancelIsClearedAfterTheTurn keeps handleInterrupt honest between
// turns: a stale cancel left behind would make Stop on an idle session report a
// cancellation and re-cancel a context nobody is waiting on.
func TestTurnCancelIsClearedAfterTheTurn(t *testing.T) {
	_, restore := captureEvents(t)
	defer restore()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	h := newTestHarness(&Config{OpenClawURL: server.URL})
	h.agentID = "main"

	if err := h.sendMessage("a short turn"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}

	h.turnMu.Lock()
	defer h.turnMu.Unlock()
	if h.turnCancel != nil {
		t.Error("turnCancel survived the turn that installed it")
	}
}
