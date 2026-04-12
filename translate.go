package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

const harness = msg.HarnessOpenClaw

// OpenClaw JSONL types

type jsonlEntry struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

type jsonlMessage struct {
	Role       string          `json:"role"`
	ToolName   string          `json:"toolName,omitempty"`
	StopReason string          `json:"stopReason,omitempty"`
	Content    json.RawMessage `json:"content"`
	Model      string          `json:"model,omitempty"`
	Provider   string          `json:"provider,omitempty"`
	Usage      *ocUsage        `json:"usage,omitempty"`
}

type ocUsage struct {
	Input      int     `json:"input"`
	Output     int     `json:"output"`
	CacheRead  int     `json:"cacheRead"`
	CacheWrite int     `json:"cacheWrite"`
	Total      int     `json:"totalTokens"`
	Cost       *ocCost `json:"cost"`
}

type ocCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// translateEntry converts a JSONL entry into canonical msg.Event(s).
func translateEntry(raw []byte, sessionID string, agg *UsageAggregator) []msg.Event {
	var entry jsonlEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil
	}
	if entry.Type != "message" {
		return nil
	}

	var m jsonlMessage
	if err := json.Unmarshal(entry.Message, &m); err != nil {
		return nil
	}

	switch m.Role {
	case "assistant":
		return translateAssistant(m, sessionID, raw, agg)
	case "toolResult":
		return translateToolResult(m, sessionID, raw)
	default:
		return nil // skip user messages
	}
}

func translateAssistant(m jsonlMessage, sessionID string, raw []byte, agg *UsageAggregator) []msg.Event {
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}

	// Record usage if present.
	if m.Usage != nil {
		agg.AddAPICall(*m.Usage)
	}

	var events []msg.Event
	rawMsg := json.RawMessage(raw)

	for i, block := range blocks {
		switch block.Type {
		case "thinking":
			text := block.Text
			if text == "" {
				text = block.Thinking
			}
			if text == "" {
				continue
			}
			events = append(events, makeEvent(sessionID, msg.EventThinking, rawMsg, func(e *msg.Event) {
				e.Thinking = &msg.ThinkingEvent{Text: text}
			}))
			events = append(events, makeEvent(sessionID, msg.EventStream, rawMsg, func(e *msg.Event) {
				e.Stream = &msg.HarnessStream{
					Delta: &msg.BlockDelta{
						Index:    i,
						Type:     msg.DeltaThinking,
						Thinking: text,
					},
				}
			}))

		case "toolCall":
			agg.AddToolCall()
			var toolInput json.RawMessage
			if len(block.Arguments) > 0 {
				toolInput = block.Arguments
			}
			events = append(events, makeEvent(sessionID, msg.EventToolCall, rawMsg, func(e *msg.Event) {
				e.ToolCall = &msg.ToolCallEvent{
					ToolID: block.ID,
					Name:   block.Name,
					Input:  toolInput,
				}
			}))

		case "text":
			if block.Text == "" {
				continue
			}
			events = append(events, makeEvent(sessionID, msg.EventStream, rawMsg, func(e *msg.Event) {
				e.Stream = &msg.HarnessStream{
					Delta: &msg.BlockDelta{
						Index: i,
						Type:  msg.DeltaText,
						Text:  block.Text,
					},
					Hidden: isHiddenOutbound(block.Text),
				}
			}))
		}
	}

	// Detect turn completion.
	if m.StopReason == "stop" {
		// Extract final text for the result event.
		var resultText string
		for _, block := range blocks {
			if block.Type == "text" && block.Text != "" {
				resultText = block.Text
			}
		}

		usage, cost := agg.Finalize(m)

		events = append(events, makeEvent(sessionID, msg.EventResult, rawMsg, func(e *msg.Event) {
			e.Result = &msg.ResultEvent{
				Text:          resultText,
				Usage:         usage,
				Cost:          cost,
				Model:         m.Model,
				APICalls:      len(agg.APICallUsages()),
				APICallUsages: agg.APICallUsages(),
			}
		}))
		events = append(events, makeEvent(sessionID, msg.EventSessionState, rawMsg, func(e *msg.Event) {
			e.State = &msg.StateEvent{State: msg.SessionIdle, Previous: msg.SessionRunning}
		}))

		agg.Reset()
	}

	return events
}

func translateToolResult(m jsonlMessage, sessionID string, raw []byte) []msg.Event {
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}

	rawMsg := json.RawMessage(raw)
	var events []msg.Event
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			events = append(events, makeEvent(sessionID, msg.EventToolResult, rawMsg, func(e *msg.Event) {
				e.ToolResult = &msg.ToolResultEvent{
					Name:   m.ToolName,
					Output: truncate(block.Text, 500),
				}
			}))
		}
	}
	return events
}

// isHiddenOutbound returns true for outbound messages that should be hidden
// (heartbeat acks, silent replies, bare tool-call labels, etc).
func isHiddenOutbound(text string) bool {
	upper := strings.TrimSpace(strings.ToUpper(text))
	switch upper {
	case "HEARTBEAT_OK", "NO_REPLY", "API CALL", "API_CALL", "TOOL CALL", "TOOL_CALL":
		return true
	}
	return false
}

// makeEvent builds a canonical msg.Event with common fields set.
func makeEvent(sessionID string, eventType msg.EventType, raw json.RawMessage, fill func(*msg.Event)) msg.Event {
	e := msg.Event{
		Type:      eventType,
		Harness:   harness,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Raw:       raw,
	}
	fill(&e)
	return e
}
