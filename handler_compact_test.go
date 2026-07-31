package main

import (
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// TestHandleCompactRefusesLoudly pins the whole refusal, because each half on
// its own is still a lie:
//
//   - the returned error is what the JSON-RPC caller sees. Without it,
//     POST /sessions/{id}/compact answers 200 and the request looks performed.
//   - the emitted error event is what a chat UI sees. Without it, a user
//     watching the transcript sees a compact request produce nothing at all.
//
// The handler previously had neither: it emitted a system "compact_ack"
// reading "compaction delegated to OpenClaw" and returned nil, having written
// nothing to OpenClaw.
func TestHandleCompactRefusesLoudly(t *testing.T) {
	events, restore := captureEvents(t)
	defer restore()

	h := &Harness{bridgeSessionID: "bs_1"}
	err := h.handleCompact(CompactParams{})

	if err == nil {
		t.Fatal("handleCompact returned nil — the caller is told a compaction succeeded")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("err = %v, want it to say the method is unsupported", err)
	}

	got := events()
	if len(got) != 1 {
		t.Fatalf("emitted %d events, want exactly 1 (the refusal)", len(got))
	}
	ev := got[0]
	if ev.Type != msg.EventError {
		t.Fatalf("event type = %q, want %q", ev.Type, msg.EventError)
	}
	if ev.Error == nil {
		t.Fatal("error event carries no ErrorEvent payload")
	}
	if ev.Error.Code != "UNSUPPORTED" {
		t.Errorf("error code = %q, want %q", ev.Error.Code, "UNSUPPORTED")
	}
	if ev.Error.Retryable {
		t.Error("refusal is marked retryable — retrying an unimplemented method cannot help")
	}
	if ev.BridgeSessionID != "bs_1" {
		t.Errorf("bridge_session_id = %q, want it stamped from the harness", ev.BridgeSessionID)
	}
}

// TestHandleCompactClaimsNoDelegation guards the specific wording that made
// this defect survive review: "delegated to OpenClaw" reads as work handed
// off, and nothing was handed anywhere.
func TestHandleCompactClaimsNoDelegation(t *testing.T) {
	events, restore := captureEvents(t)
	defer restore()

	h := &Harness{bridgeSessionID: "bs_1"}
	_ = h.handleCompact(CompactParams{})

	for _, ev := range events() {
		if ev.System != nil && strings.Contains(ev.System.Message, "delegated") {
			t.Errorf("emitted %q — nothing is delegated; the handler makes no call to OpenClaw", ev.System.Message)
		}
		if ev.Error != nil && strings.Contains(ev.Error.Message, "delegated") {
			t.Errorf("refusal message %q claims a delegation", ev.Error.Message)
		}
	}
}
