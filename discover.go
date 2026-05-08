package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// discoverScope filters discoveredSession results to those the OpenClaw
// harness can actually resume today. handler.go hardcodes sessionName="main"
// (handler.go:231,238), so subagent / cron / hook sessions on disk surface
// for visibility but cannot yet be resumed; for the first conformance cut
// we restrict discover to the canonical "main" session per agent. Once
// handler.go grows multi-session support this can widen.
const discoverScopeMain = "main"

// emitDiscover prints a JSON array of msg.StoredSession to stdout, one entry
// per `agent:<agent>:main` session found under cfg.OpenClawDir. On any error
// (dir missing, sessions.json malformed) it logs to stderr and prints "[]"
// — same contract shape as the hermes/cline empty cases.
func emitDiscover(cfg *Config) {
	sessions := collectStoredSessions(cfg.OpenClawDir)
	if sessions == nil {
		sessions = []msg.StoredSession{}
	}
	if err := json.NewEncoder(os.Stdout).Encode(sessions); err != nil {
		fmt.Fprintf(os.Stderr, "discover: encode: %v\n", err)
		fmt.Println("[]")
	}
}

// collectStoredSessions walks the OpenClaw on-disk session tree and returns
// canonical StoredSession entries for each `agent:<agent>:main` session.
//
// HarnessSessionID is `<agent>:main` — stable across JSONL UUID rotations
// (deleting and recreating a session swaps the UUID but keeps the name) and
// directly matches the harness's resume key (StartParams.AgentID + the
// implicit sessionName="main"). BridgeSessionID is left empty so
// bridge-server's adoption path can mint or reuse the chain head.
func collectStoredSessions(openclawDir string) []msg.StoredSession {
	if openclawDir == "" {
		return nil
	}
	discovered := discoverAllSessions(openclawDir)
	if len(discovered) == 0 {
		return nil
	}

	out := make([]msg.StoredSession, 0, len(discovered))
	for _, d := range discovered {
		if d.sessionName != discoverScopeMain {
			continue
		}
		entry := storedSessionFor(openclawDir, d)
		out = append(out, entry)
	}
	return out
}

// storedSessionFor maps a single discoveredSession to msg.StoredSession,
// filling timestamps from sessions.json's updatedAt when available and
// falling back to the JSONL file mtime/btime otherwise.
func storedSessionFor(openclawDir string, d discoveredSession) msg.StoredSession {
	updated := updatedAtFromSessionsJSON(openclawDir, d.agent, d.sessionName)
	if updated.IsZero() {
		if info, err := os.Stat(d.path); err == nil {
			updated = info.ModTime()
		}
	}
	created := updated // OpenClaw doesn't track creation separately; mtime is the best proxy.

	return msg.StoredSession{
		HarnessSessionID: d.agent + ":" + d.sessionName,
		Harness:          msg.HarnessOpenClaw,
		Project:          d.agent,
		Prompt:           d.sessionName,
		CreatedAt:        created,
		UpdatedAt:        updated,
		Path:             d.path,
	}
}

// updatedAtFromSessionsJSON reads `updatedAt` (ms epoch) for the
// `agent:<agent>:<sessionName>` key in the per-agent sessions.json. Returns
// zero time when the key is missing or the file can't be read; caller
// should fall back to JSONL mtime.
func updatedAtFromSessionsJSON(openclawDir, agent, sessionName string) time.Time {
	sessFile := filepath.Join(openclawDir, "agents", agent, "sessions", "sessions.json")
	data, err := os.ReadFile(sessFile)
	if err != nil {
		return time.Time{}
	}
	var raw map[string]struct {
		UpdatedAt int64 `json:"updatedAt"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return time.Time{}
	}
	entry, ok := raw["agent:"+agent+":"+sessionName]
	if !ok || entry.UpdatedAt == 0 {
		return time.Time{}
	}
	return time.UnixMilli(entry.UpdatedAt).UTC()
}
