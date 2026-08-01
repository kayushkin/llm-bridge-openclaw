package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// openaiRequest is the OpenAI-compatible request format for OpenClaw.
type openaiRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream,omitempty"`
	StreamOptions *streamOpts   `json:"stream_options,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// sendToOpenClaw sends a message to the OpenClaw REST API and consumes the SSE stream.
// The actual event content comes from JSONL tailing, not the SSE stream.
func sendToOpenClaw(ctx context.Context, cfg *Config, agentID, sessionName, content string) error {
	ocSessionKey := "agent:" + agentID + ":" + sessionName

	reqBody := openaiRequest{
		Model:         "openclaw",
		Messages:      []chatMessage{{Role: "user", Content: content}},
		Stream:        true,
		StreamOptions: &streamOpts{IncludeUsage: true},
	}
	data, _ := json.Marshal(reqBody)

	url := strings.TrimRight(cfg.OpenClawURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	req.Header.Set("x-openclaw-scopes", "operator.write")
	req.Header.Set("x-openclaw-agent-id", agentID)
	req.Header.Set("x-openclaw-session-key", ocSessionKey)

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	// Consume the SSE stream to keep the connection alive.
	// All event publishing comes from the JSONL tailer.
	//
	// Match the 10MB line cap the other live SSE readers use (hermes
	// client.go, kilocode sseclient.go): a single SSE frame above bufio's
	// 64KB default would otherwise end Scan() indistinguishably from a
	// closed stream, silently truncating the drain.
	sseScanner := bufio.NewScanner(resp.Body)
	sseScanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for sseScanner.Scan() {
		line := sseScanner.Text()
		if strings.HasPrefix(line, "data: [DONE]") {
			break
		}
	}
	// Fail loud: a swallowed scanner error (an over-cap line, a broken
	// connection, an interrupted turn) is otherwise indistinguishable from a
	// clean stream end. Logging it was not enough — this function's return
	// value is the caller's only signal, so a stream that died mid-turn was
	// still reported to the harness as a turn that finished. That mattered the
	// moment an interrupt became possible: cancelling the turn context aborts
	// the read here, and the caller has to see a cancellation rather than a
	// completed turn. Scan stops at EOF with a nil Err, so a stream that ends
	// without [DONE] is still the clean case it always was.
	if err := sseScanner.Err(); err != nil {
		log.Printf("SSE stream read error for agent=%s session=%s: %v", agentID, sessionName, err)
		return fmt.Errorf("sse stream: %w", err)
	}
	log.Printf("SSE stream ended for agent=%s session=%s", agentID, sessionName)

	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
