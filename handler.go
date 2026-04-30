package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Request is the JSON-RPC request format from llm-bridge.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// StartParams are the parameters for the "start" method.
type StartParams struct {
	BridgeSessionID  string `json:"bridge_session_id,omitempty"`
	HarnessSessionID string `json:"harness_session_id,omitempty"`
	// Deprecated: use BridgeSessionID + HarnessSessionID.
	SessionID   string `json:"session_id"`
	DisplayName string `json:"display_name"`
	AgentID     string `json:"agent_id"`
	Prompt      string `json:"prompt"`
	Resume      bool   `json:"resume"`
	Fork        string `json:"fork"`
}

// MessageParams are the parameters for the "message" method.
type MessageParams struct {
	Content string `json:"content"`
}

// CompactParams are the parameters for the "compact" method.
type CompactParams struct {
	Summary string `json:"summary"`
}

// Harness holds the runtime state for a single harness session.
type Harness struct {
	cfg       *Config
	sessionID string
	agentID   string
	agg       UsageAggregator
	ctx       context.Context
	cancel    context.CancelFunc
	tailer    *Tailer
}

// NewHarness creates a new harness instance.
func NewHarness(cfg *Config) *Harness {
	ctx, cancel := context.WithCancel(context.Background())
	return &Harness{cfg: cfg, ctx: ctx, cancel: cancel}
}

// HandleRequest dispatches a JSON-RPC request to the appropriate handler.
func (h *Harness) HandleRequest(req Request) error {
	switch req.Method {
	case "start":
		var params StartParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return fmt.Errorf("parse start params: %w", err)
		}
		return h.handleStart(params)

	case "message":
		var params MessageParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return fmt.Errorf("parse message params: %w", err)
		}
		return h.handleMessage(params)

	case "compact":
		var params CompactParams
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &params)
		}
		return h.handleCompact(params)

	case "resume":
		return h.handleResume()

	default:
		return fmt.Errorf("unknown method: %s", req.Method)
	}
}

// handleStart initializes the session and starts tailing the JSONL file.
func (h *Harness) handleStart(params StartParams) error {
	if params.HarnessSessionID != "" {
		h.sessionID = params.HarnessSessionID
	} else {
		h.sessionID = params.SessionID
	}
	h.agentID = params.AgentID
	if h.agentID == "" {
		h.agentID = "main"
	}

	// Emit running state.
	emitEvent(msg.Event{
		Type:      msg.EventSessionState,
		Harness:   harness,
		HarnessSessionID: h.sessionID,
		Timestamp: time.Now(),
		State:     &msg.StateEvent{State: msg.SessionRunning, Previous: msg.SessionIdle},
	})
	emitEvent(msg.Event{
		Type:      msg.EventSystem,
		Harness:   harness,
		HarnessSessionID: h.sessionID,
		Timestamp: time.Now(),
		System:    &msg.SystemEvent{Subtype: "init", Message: "openclaw_url=" + h.cfg.OpenClawURL + " agent=" + h.agentID},
	})

	// Start JSONL tailer if OpenClaw dir is configured.
	if h.cfg.OpenClawDir != "" {
		h.startTailer()
	}

	// Send the initial prompt if provided.
	if params.Prompt != "" {
		if err := h.sendMessage(params.Prompt); err != nil {
			emitEvent(msg.Event{
				Type:      msg.EventError,
				Harness:   harness,
				HarnessSessionID: h.sessionID,
				Timestamp: time.Now(),
				Error: &msg.ErrorEvent{
					Code:    "SEND_ERROR",
					Message: err.Error(),
				},
			})
			return err
		}
	}

	return nil
}

// handleMessage sends a follow-up message to the OpenClaw session.
func (h *Harness) handleMessage(params MessageParams) error {
	// If tailer isn't running, start it.
	if h.tailer == nil && h.cfg.OpenClawDir != "" {
		h.startTailer()
	}

	if err := h.sendMessage(params.Content); err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	return nil
}

// handleCompact acknowledges a compact request. OpenClaw manages compaction internally.
func (h *Harness) handleCompact(params CompactParams) error {
	emitEvent(msg.Event{
		Type:      msg.EventSystem,
		Harness:   harness,
		HarnessSessionID: h.sessionID,
		Timestamp: time.Now(),
		System:    &msg.SystemEvent{Subtype: "compact_ack", Message: "compaction delegated to OpenClaw"},
	})
	return nil
}

// handleResume restarts the tailer if it's not running.
func (h *Harness) handleResume() error {
	if h.tailer == nil && h.cfg.OpenClawDir != "" {
		h.startTailer()
	}
	return nil
}

// sendMessage forwards a message to OpenClaw via the REST API.
func (h *Harness) sendMessage(content string) error {
	sessionName := "main"

	return sendToOpenClaw(h.ctx, h.cfg, h.agentID, sessionName, content)
}

// startTailer starts the JSONL file tailer in the background.
func (h *Harness) startTailer() {
	sessionName := "main"
	sessPath, err := findSessionFile(h.cfg.OpenClawDir, h.agentID, sessionName)
	if err != nil {
		log.Printf("session file not found: %v", err)
		return
	}

	h.tailer = NewTailer(sessPath, h.sessionID, &h.agg)
	go h.tailer.Run(h.ctx)
	log.Printf("started JSONL tailer for %s", sessPath)
}

// Shutdown cleans up the harness.
func (h *Harness) Shutdown() {
	h.cancel()
}
