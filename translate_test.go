package main

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestMakeEvent_StampsBothIDs(t *testing.T) {
	e := makeEvent("bs_1", "h_1", msg.EventBlock, json.RawMessage(`{}`), func(e *msg.Event) {
		e.Block = &msg.BlockEvent{Block: &msg.ContentBlock{
			Type: msg.BlockText,
			Text: &msg.TextBlock{Text: "hello"},
		}}
	})

	if e.BridgeSessionID != "bs_1" {
		t.Errorf("BridgeSessionID = %q, want bs_1", e.BridgeSessionID)
	}
	if e.HarnessSessionID != "h_1" {
		t.Errorf("HarnessSessionID = %q, want h_1", e.HarnessSessionID)
	}
	if e.Harness != msg.HarnessOpenClaw {
		t.Errorf("Harness = %q, want openclaw", e.Harness)
	}
	if e.Type != msg.EventBlock {
		t.Errorf("Type = %q, want block", e.Type)
	}
	if e.Block == nil || e.Block.Block == nil || e.Block.Block.Text == nil || e.Block.Block.Text.Text != "hello" {
		t.Errorf("Block fill not applied: %+v", e)
	}
	if e.Timestamp.IsZero() {
		t.Errorf("Timestamp not set")
	}
}

func TestTranslateAssistant_StampsBothIDs(t *testing.T) {
	rawMsg := []byte(`{
		"type":"message",
		"message":{
			"role":"assistant",
			"model":"openclaw",
			"stopReason":"stop",
			"content":[
				{"type":"thinking","thinking":"reasoning..."},
				{"type":"text","text":"hello"}
			],
			"usage":{
				"input":10,"output":20,"totalTokens":30,
				"cost":{"total":0.001}
			}
		}
	}`)

	agg := &UsageAggregator{}
	events := translateEntry(rawMsg, "bs_2", "h_2", agg)

	if len(events) < 3 {
		t.Fatalf("expected thinking + text + result + state events, got %d", len(events))
	}
	for i, e := range events {
		if e.BridgeSessionID != "bs_2" {
			t.Errorf("event[%d].BridgeSessionID = %q, want bs_2 (type=%s)", i, e.BridgeSessionID, e.Type)
		}
		if e.HarnessSessionID != "h_2" {
			t.Errorf("event[%d].HarnessSessionID = %q, want h_2 (type=%s)", i, e.HarnessSessionID, e.Type)
		}
	}

	// First two events: thinking block + text block
	if events[0].Type != msg.EventBlock || events[0].Block == nil || events[0].Block.Block.Type != msg.BlockThinking {
		t.Errorf("events[0] = %+v, want EventBlock thinking", events[0])
	}
	if events[1].Type != msg.EventBlock || events[1].Block == nil || events[1].Block.Block.Type != msg.BlockText {
		t.Errorf("events[1] = %+v, want EventBlock text", events[1])
	}
	// Third: result event with the final text
	if events[2].Type != msg.EventResult || events[2].Result == nil || events[2].Result.Text != "hello" {
		t.Errorf("events[2] = %+v, want EventResult text=hello", events[2])
	}
	// Fourth: state -> idle
	if events[3].Type != msg.EventSessionState || events[3].State == nil || events[3].State.State != msg.SessionIdle {
		t.Errorf("events[3] = %+v, want EventSessionState idle", events[3])
	}
}

func TestTranslateToolResult_StampsBothIDs(t *testing.T) {
	rawMsg := []byte(`{
		"type":"message",
		"message":{
			"role":"toolResult",
			"toolName":"shell",
			"content":[{"type":"text","text":"ok"}]
		}
	}`)

	events := translateEntry(rawMsg, "bs_3", "h_3", &UsageAggregator{})

	if len(events) != 1 {
		t.Fatalf("expected 1 tool_result event, got %d", len(events))
	}
	e := events[0]
	if e.Type != msg.EventToolResult {
		t.Errorf("type = %q, want tool_result", e.Type)
	}
	if e.ToolResult == nil || e.ToolResult.Name != "shell" || e.ToolResult.Output != "ok" {
		t.Errorf("ToolResult = %+v, want {Name:shell Output:ok}", e.ToolResult)
	}
	if e.BridgeSessionID != "bs_3" {
		t.Errorf("BridgeSessionID = %q, want bs_3", e.BridgeSessionID)
	}
	if e.HarnessSessionID != "h_3" {
		t.Errorf("HarnessSessionID = %q, want h_3", e.HarnessSessionID)
	}
}

func TestTranslateAssistant_HiddenOutboundSkipped(t *testing.T) {
	rawMsg := []byte(`{
		"type":"message",
		"message":{
			"role":"assistant",
			"content":[{"type":"text","text":"HEARTBEAT_OK"}]
		}
	}`)

	events := translateEntry(rawMsg, "bs", "hs", &UsageAggregator{})
	if len(events) != 0 {
		t.Errorf("expected no events for HEARTBEAT_OK, got %d", len(events))
	}
}

func TestTranslateUserMessage_Skipped(t *testing.T) {
	rawMsg := []byte(`{
		"type":"message",
		"message":{
			"role":"user",
			"content":[{"type":"text","text":"hi"}]
		}
	}`)

	events := translateEntry(rawMsg, "bs", "hs", &UsageAggregator{})
	if len(events) != 0 {
		t.Errorf("expected no events for user message, got %d", len(events))
	}
}
