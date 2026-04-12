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
	sseScanner := bufio.NewScanner(resp.Body)
	for sseScanner.Scan() {
		line := sseScanner.Text()
		if strings.HasPrefix(line, "data: [DONE]") {
			break
		}
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
