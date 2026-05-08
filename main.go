package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

const version = "0.1.0"

// emitMu guards writes so concurrent goroutines don't interleave JSON lines.
var emitMu sync.Mutex

// emitSink receives the NDJSON event stream. Defaults to os.Stdout (the
// llm-bridge subprocess contract); tests swap it for a captured buffer.
var emitSink io.Writer = os.Stdout

// emitEvent writes a canonical msg.Event as a single NDJSON line to emitSink.
func emitEvent(ev any) {
	emitMu.Lock()
	defer emitMu.Unlock()

	data, err := json.Marshal(ev)
	if err != nil {
		log.Printf("failed to marshal event: %v", err)
		return
	}
	data = append(data, '\n')
	emitSink.Write(data)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println(version)
		os.Exit(0)
	}

	// -discover walks OPENCLAW_DIR/agents/*/sessions/ and prints a JSON array
	// of canonical msg.StoredSession to stdout. On any error (dir missing,
	// malformed sessions.json) it falls back to "[]" — contract-correct
	// "no discoverable sessions" matches the cline / hermes empty shape.
	if len(os.Args) > 1 && os.Args[1] == "-discover" {
		emitDiscover(loadConfig())
		os.Exit(0)
	}

	// -import-history is part of the conformance contract but not yet
	// implemented for openclaw. Exit 2 to signal "unsupported" rather than
	// silently falling through to the JSON-RPC loop, which would otherwise
	// show up as a false-positive PASS on the conformance dashboard.
	if len(os.Args) > 1 && os.Args[1] == "-import-history" {
		fmt.Fprintln(os.Stderr, "llm-bridge-openclaw: -import-history not yet implemented")
		os.Exit(2)
	}

	log.SetOutput(os.Stderr)
	log.SetPrefix("[llm-bridge-openclaw] ")

	cfg := loadConfig()
	h := NewHarness(cfg)

	// Handle signals for graceful shutdown.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Printf("received %v, shutting down", sig)
		h.Shutdown()
		os.Exit(0)
	}()

	// Read JSON-RPC requests from llm-bridge on stdin.
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("invalid request: %v (line: %s)", err, string(line))
			continue
		}

		log.Printf("request: method=%s", req.Method)
		if err := h.HandleRequest(req); err != nil {
			log.Printf("handler error: method=%s err=%v", req.Method, err)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("stdin scanner error: %v", err)
	}

	// stdin closed — llm-bridge is done with us.
	log.Printf("stdin closed, shutting down")
	h.Shutdown()
}
