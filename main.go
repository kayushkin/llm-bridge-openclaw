package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/kayushkin/llm-bridge/ndjson"
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

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go watchSignals(h, sigs, func() { os.Exit(0) })

	// Read JSON-RPC requests from llm-bridge on stdin.
	// ndjson.ReadLine carries no practical line cap and reports an oversized
	// line as its own error, so a single large request (a pasted image, a large
	// tool result) no longer looks like a closed stdin and kills the session —
	// the failure mode of the old bufio.Scanner, whose over-cap line ended the
	// scan indistinguishably from EOF.
	reader := bufio.NewReader(os.Stdin)

	for {
		line, readErr := ndjson.ReadLine(reader, ndjson.MaxLineBytes)
		if errors.Is(readErr, ndjson.ErrLineTooLong) {
			log.Printf("dropping request line above %d bytes; session continues", ndjson.MaxLineBytes)
			continue
		}
		if len(line) == 0 {
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					log.Printf("stdin read error: %v", readErr)
				}
				break
			}
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

	// stdin closed — llm-bridge is done with us.
	log.Printf("stdin closed, shutting down")
	h.Shutdown()
}

// watchSignals runs this bridge's interrupt contract until the signal channel
// closes: SIGINT cancels the in-flight turn and the session carries on, any
// other signal shuts the session down.
//
// SIGINT used to exit the process, so pressing Stop in a chat killed the whole
// openclaw session rather than the turn the user wanted to stop.
//
// It must be a loop rather than a single read. llm-bridge-server's Stop is a
// SIGINT and there is no JSON-RPC interrupt method, so a handler that reads one
// signal honours the first Stop of a session and silently ignores every Stop
// after it — which is how the same defect hid in aider, forgecode and nanoclaw.
// It also lives here, out of the goroutine literal it used to be written in,
// because a handler observable only by dying cannot be tested.
//
// terminate is a parameter so a test can watch the shutdown path without the
// process exiting underneath it.
func watchSignals(h *Harness, sigs <-chan os.Signal, terminate func()) {
	for sig := range sigs {
		if sig == syscall.SIGINT {
			log.Printf("received %v, cancelling the in-flight turn", sig)
			if err := h.handleInterrupt(); err != nil {
				log.Printf("interrupt: %v", err)
			}
			continue
		}
		log.Printf("received %v, shutting down", sig)
		h.Shutdown()
		terminate()
		return
	}
}
