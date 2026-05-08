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
	SessionID   string `json:"session_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	Resume      bool   `json:"resume,omitempty"`
	// Fork carries the parent's harness_session_id when the bridge-server
	// forks a session. OpenClaw has no native session-cloning primitive, so
	// fork-via-start is rejected with an explicit FORK_UNSUPPORTED error.
	Fork string `json:"fork,omitempty"`
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
//
// Two session ids flow through every event:
//   - bridgeSessionID is the caller's stable id (bridge-server's routing key);
//     resolved from params.BridgeSessionID, with params.SessionID as legacy
//     fallback for callers that have not migrated yet.
//   - harnessSessionID is the OpenClaw-side id. OpenClaw does not surface its
//     own session id back through the OpenAI-compatible REST API, so this is
//     pinned to bridgeSessionID for the lifetime of the harness; downstream
//     tools can use either id interchangeably.
type Harness struct {
	cfg              *Config
	bridgeSessionID  string
	harnessSessionID string
	agentID          string
	agg              UsageAggregator
	ctx              context.Context
	cancel           context.CancelFunc
	tailer           *Tailer
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
//
// The session-chain contract sends `start` with one of three intents:
//   - cold start (Resume=false, Fork=""): allocate a fresh openclaw session
//   - resume (Resume=true): re-bind to an existing openclaw session by id
//   - fork (Fork=<parent harness id>): not supported on openclaw — emit
//     EventError{FORK_UNSUPPORTED} and return. OpenClaw has no native
//     session-cloning primitive, so pretending to fork would silently produce
//     a fresh chain with no inherited state.
func (h *Harness) handleStart(params StartParams) error {
	h.bridgeSessionID = resolveBridgeSessionID(params)
	h.harnessSessionID = resolveHarnessSessionID(params, h.bridgeSessionID)
	h.agentID = params.AgentID
	if h.agentID == "" {
		h.agentID = "main"
	}

	if params.Fork != "" {
		h.emit(msg.EventError, func(e *msg.Event) {
			e.Error = &msg.ErrorEvent{
				Code:      "FORK_UNSUPPORTED",
				Message:   "openclaw has no native session-cloning primitive; fork-via-start is not supported",
				Retryable: false,
			}
		})
		return fmt.Errorf("fork unsupported")
	}

	initSubtype := "init"
	if params.Resume {
		initSubtype = "resume"
	}
	h.emit(msg.EventSystem, func(e *msg.Event) {
		e.System = &msg.SystemEvent{
			Subtype: initSubtype,
			Message: "openclaw_url=" + h.cfg.OpenClawURL + " agent=" + h.agentID,
		}
	})

	// Start JSONL tailer if OpenClaw dir is configured.
	if h.cfg.OpenClawDir != "" {
		h.startTailer()
	}

	// Send the initial prompt if provided.
	if params.Prompt != "" {
		if err := h.sendMessage(params.Prompt); err != nil {
			h.emit(msg.EventError, func(e *msg.Event) {
				e.Error = &msg.ErrorEvent{
					Code:    "SEND_ERROR",
					Message: err.Error(),
				}
			})
			return err
		}
	}

	return nil
}

// resolveBridgeSessionID picks BridgeSessionID, falling back to the legacy
// SessionID field for callers that have not migrated yet.
func resolveBridgeSessionID(p StartParams) string {
	if p.BridgeSessionID != "" {
		return p.BridgeSessionID
	}
	return p.SessionID
}

// resolveHarnessSessionID picks HarnessSessionID, falling back to legacy
// SessionID, then to the resolved bridge session id. OpenClaw does not return
// its own session id, so the harness side stays pinned to bridgeSessionID
// when the caller did not specify one.
func resolveHarnessSessionID(p StartParams, bridgeSessionID string) string {
	if p.HarnessSessionID != "" {
		return p.HarnessSessionID
	}
	if p.SessionID != "" {
		return p.SessionID
	}
	return bridgeSessionID
}

// emit writes a msg.Event with both session ids stamped from the harness.
func (h *Harness) emit(eventType msg.EventType, fill func(*msg.Event)) {
	e := msg.Event{
		Type:             eventType,
		Harness:          harness,
		BridgeSessionID:  h.bridgeSessionID,
		HarnessSessionID: h.harnessSessionID,
		Timestamp:        time.Now(),
	}
	fill(&e)
	emitEvent(e)
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
	h.emit(msg.EventSystem, func(e *msg.Event) {
		e.System = &msg.SystemEvent{Subtype: "compact_ack", Message: "compaction delegated to OpenClaw"}
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

	h.tailer = NewTailer(sessPath, h.bridgeSessionID, h.harnessSessionID, &h.agg)
	go h.tailer.Run(h.ctx)
	log.Printf("started JSONL tailer for %s", sessPath)
}

// Shutdown cleans up the harness.
func (h *Harness) Shutdown() {
	h.cancel()
}
