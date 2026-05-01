package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// captureEvents swaps emitSink for a bytes.Buffer for the duration of the
// test, returning a function that decodes the captured NDJSON into Events.
func captureEvents(t *testing.T) (events func() []msg.Event, restore func()) {
	t.Helper()

	emitMu.Lock()
	prev := emitSink
	buf := &bytes.Buffer{}
	emitSink = buf
	emitMu.Unlock()

	return func() []msg.Event {
			emitMu.Lock()
			defer emitMu.Unlock()
			var out []msg.Event
			for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
				if line == "" {
					continue
				}
				var e msg.Event
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					t.Fatalf("decode emitted event: %v (line=%q)", err, line)
				}
				out = append(out, e)
			}
			return out
		}, func() {
			emitMu.Lock()
			emitSink = prev
			emitMu.Unlock()
		}
}

func TestResolveBridgeSessionID(t *testing.T) {
	cases := []struct {
		name string
		p    StartParams
		want string
	}{
		{"bridge_session_id wins", StartParams{BridgeSessionID: "bs_1", SessionID: "legacy"}, "bs_1"},
		{"legacy session_id fallback", StartParams{SessionID: "legacy"}, "legacy"},
		{"empty input", StartParams{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveBridgeSessionID(c.p); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestResolveHarnessSessionID(t *testing.T) {
	cases := []struct {
		name            string
		p               StartParams
		bridgeSessionID string
		want            string
	}{
		{"harness_session_id wins", StartParams{HarnessSessionID: "h_1", SessionID: "legacy"}, "bs_1", "h_1"},
		{"legacy session_id fallback", StartParams{SessionID: "legacy"}, "bs_1", "legacy"},
		{"falls back to bridge id", StartParams{}, "bs_1", "bs_1"},
		{"all empty", StartParams{}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveHarnessSessionID(c.p, c.bridgeSessionID); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestHandleStartColdStart_StampsBothIDs(t *testing.T) {
	get, restore := captureEvents(t)
	defer restore()

	h := NewHarness(&Config{OpenClawURL: "http://example", OpenClawDir: ""})
	if err := h.handleStart(StartParams{
		BridgeSessionID:  "bs_1",
		HarnessSessionID: "h_1",
		AgentID:          "main",
	}); err != nil {
		t.Fatalf("handleStart cold start: %v", err)
	}

	if h.bridgeSessionID != "bs_1" {
		t.Errorf("h.bridgeSessionID = %q, want bs_1", h.bridgeSessionID)
	}
	if h.harnessSessionID != "h_1" {
		t.Errorf("h.harnessSessionID = %q, want h_1", h.harnessSessionID)
	}

	events := get()
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events (state + system), got %d", len(events))
	}
	for i, e := range events {
		if e.BridgeSessionID != "bs_1" {
			t.Errorf("event[%d].BridgeSessionID = %q, want bs_1 (type=%s)", i, e.BridgeSessionID, e.Type)
		}
		if e.HarnessSessionID != "h_1" {
			t.Errorf("event[%d].HarnessSessionID = %q, want h_1 (type=%s)", i, e.HarnessSessionID, e.Type)
		}
		if e.Harness != msg.HarnessOpenClaw {
			t.Errorf("event[%d].Harness = %q, want openclaw", i, e.Harness)
		}
	}

	if events[0].Type != msg.EventSessionState {
		t.Errorf("first event = %q, want session_state", events[0].Type)
	}
	if events[1].Type != msg.EventSystem || events[1].System == nil || events[1].System.Subtype != "init" {
		t.Errorf("second event = %v, want system{init}", events[1])
	}
}

func TestHandleStartResume_EmitsResumeSystemEvent(t *testing.T) {
	get, restore := captureEvents(t)
	defer restore()

	h := NewHarness(&Config{OpenClawURL: "http://example"})
	if err := h.handleStart(StartParams{
		BridgeSessionID:  "bs_1",
		HarnessSessionID: "h_xyz",
		AgentID:          "main",
		Resume:           true,
	}); err != nil {
		t.Fatalf("handleStart resume: %v", err)
	}

	events := get()
	var sys *msg.SystemEvent
	for _, e := range events {
		if e.Type == msg.EventSystem {
			sys = e.System
		}
	}
	if sys == nil {
		t.Fatalf("expected a system event on resume, got none (events=%d)", len(events))
	}
	if sys.Subtype != "resume" {
		t.Errorf("system subtype = %q, want resume", sys.Subtype)
	}

	if h.harnessSessionID != "h_xyz" {
		t.Errorf("h.harnessSessionID = %q, want h_xyz", h.harnessSessionID)
	}
}

func TestHandleStartFork_EmitsForkUnsupported(t *testing.T) {
	get, restore := captureEvents(t)
	defer restore()

	h := NewHarness(&Config{OpenClawURL: "http://example"})
	err := h.handleStart(StartParams{
		BridgeSessionID: "bs_1",
		AgentID:         "main",
		Fork:            "h_parent",
	})
	if err == nil {
		t.Fatal("expected error from fork-via-start, got nil")
	}

	events := get()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event (the FORK_UNSUPPORTED error), got %d", len(events))
	}
	e := events[0]
	if e.Type != msg.EventError {
		t.Errorf("event type = %q, want error", e.Type)
	}
	if e.Error == nil || e.Error.Code != "FORK_UNSUPPORTED" {
		t.Errorf("error code = %v, want FORK_UNSUPPORTED", e.Error)
	}
	if e.BridgeSessionID != "bs_1" {
		t.Errorf("event.BridgeSessionID = %q, want bs_1", e.BridgeSessionID)
	}
	if e.Error != nil && e.Error.Retryable {
		t.Errorf("FORK_UNSUPPORTED should not be retryable")
	}
}

func TestHandleStart_LegacySessionIDFallback(t *testing.T) {
	get, restore := captureEvents(t)
	defer restore()

	h := NewHarness(&Config{OpenClawURL: "http://example"})
	if err := h.handleStart(StartParams{
		SessionID: "legacy_id",
		AgentID:   "main",
	}); err != nil {
		t.Fatalf("handleStart legacy: %v", err)
	}

	if h.bridgeSessionID != "legacy_id" {
		t.Errorf("h.bridgeSessionID = %q, want legacy_id", h.bridgeSessionID)
	}
	if h.harnessSessionID != "legacy_id" {
		t.Errorf("h.harnessSessionID = %q, want legacy_id (legacy session_id covers both slots)", h.harnessSessionID)
	}

	for _, e := range get() {
		if e.BridgeSessionID != "legacy_id" {
			t.Errorf("event %s BridgeSessionID = %q, want legacy_id", e.Type, e.BridgeSessionID)
		}
		if e.HarnessSessionID != "legacy_id" {
			t.Errorf("event %s HarnessSessionID = %q, want legacy_id", e.Type, e.HarnessSessionID)
		}
	}
}

func TestEmit_StampsBothIDs(t *testing.T) {
	get, restore := captureEvents(t)
	defer restore()

	h := &Harness{
		bridgeSessionID:  "bs_1",
		harnessSessionID: "h_1",
	}
	h.emit(msg.EventSystem, func(e *msg.Event) {
		e.System = &msg.SystemEvent{Subtype: "test", Message: "hi"}
	})

	events := get()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.BridgeSessionID != "bs_1" {
		t.Errorf("BridgeSessionID = %q, want bs_1", e.BridgeSessionID)
	}
	if e.HarnessSessionID != "h_1" {
		t.Errorf("HarnessSessionID = %q, want h_1", e.HarnessSessionID)
	}
	if e.Type != msg.EventSystem || e.System == nil || e.System.Subtype != "test" {
		t.Errorf("event = %+v", e)
	}
	if e.Timestamp.IsZero() {
		t.Errorf("Timestamp not set")
	}
}

// captureEvents must be safe for parallel tests; this guards against a future
// regression where the swap dropped the mutex.
func TestCaptureEvents_MutexProtection(t *testing.T) {
	get, restore := captureEvents(t)
	defer restore()

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := &Harness{bridgeSessionID: "bs", harnessSessionID: "hs"}
			h.emit(msg.EventSystem, func(e *msg.Event) {
				e.System = &msg.SystemEvent{Subtype: "ping"}
			})
		}()
	}
	wg.Wait()

	if got := len(get()); got != 10 {
		t.Errorf("got %d events, want 10", got)
	}
}
