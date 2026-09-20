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
	"unicode/utf8"
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
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncateAtRuneBoundaryWithEllipsis(string(body), 200))
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

// truncateAtRuneBoundaryWithEllipsis returns s unchanged when it fits in
// maxBytes, and otherwise the longest prefix of s that is no longer than
// maxBytes and does not end part-way through a multi-byte UTF-8 sequence,
// followed by an ellipsis marking that something was dropped.
//
// Cutting a Go string at a fixed byte offset splits whatever rune straddles that
// offset, and the result is not valid UTF-8. Nothing reports it: encoding/json
// substitutes U+FFFD rather than failing, so the reader sees a replacement
// character and no error is raised anywhere along the way. That matters most at
// the translate.go caller, whose result becomes msg.ToolResultEvent.Output and
// is marshalled to stdout by emitEvent for bridge-server — a split rune crosses
// to the session view and a reload does not fix it.
//
// The ellipsis sits OUTSIDE maxBytes, so a cut result is maxBytes+3 bytes. That
// is pre-existing behaviour and is deliberately left alone; whether the marker
// ought to count against the budget is a separate question from where the cut
// lands.
//
// The walk-back costs at most three byte comparisons and allocates nothing,
// which is why it is preferred here over converting to []rune.
func truncateAtRuneBoundaryWithEllipsis(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Past here a cut happens, so the slice below runs. A negative budget would
	// panic it (s[:-1]); clamp to zero, which yields the marker alone — the same
	// answer a budget of zero already gave. Neither caller can reach this today,
	// both passing a compile-time constant, but the helper is package-level and
	// a crash is a poor answer to a nonsensical budget.
	if maxBytes < 0 {
		maxBytes = 0
	}
	// s[cut] is the first byte past the prefix. While it is a continuation
	// byte, a rune straddles the cut, so move the cut earlier.
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
