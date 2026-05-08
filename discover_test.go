package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// scaffoldOpenClawDir builds a minimal OpenClaw on-disk session tree under
// dir for testing. agents is a map of agent name -> map of sessionName ->
// updatedAt (ms epoch). Each entry materializes a sessions.json key plus an
// empty <uuid>.jsonl file so discoverAllSessions() finds it.
func scaffoldOpenClawDir(t *testing.T, agents map[string]map[string]int64) string {
	t.Helper()
	dir := t.TempDir()
	for agent, sessions := range agents {
		sessDir := filepath.Join(dir, "agents", agent, "sessions")
		if err := os.MkdirAll(sessDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sessDir, err)
		}

		// Build sessions.json with one entry per session, each pointing at a
		// distinct fake UUID file.
		entries := make(map[string]map[string]any, len(sessions))
		i := 0
		for name, updatedAt := range sessions {
			uuid := agent + "-uuid-" + name + "-x"
			// Replace illegal filename chars in case sessionName has ":".
			uuid = strings.ReplaceAll(uuid, ":", "-")
			entries["agent:"+agent+":"+name] = map[string]any{
				"sessionId": uuid,
				"updatedAt": updatedAt,
			}
			jsonlPath := filepath.Join(sessDir, uuid+".jsonl")
			if err := os.WriteFile(jsonlPath, []byte(""), 0o644); err != nil {
				t.Fatalf("write jsonl: %v", err)
			}
			i++
		}
		data, _ := json.Marshal(entries)
		if err := os.WriteFile(filepath.Join(sessDir, "sessions.json"), data, 0o644); err != nil {
			t.Fatalf("write sessions.json: %v", err)
		}
	}
	return dir
}

func TestEmitDiscover_FiltersToMainScope(t *testing.T) {
	dir := scaffoldOpenClawDir(t, map[string]map[string]int64{
		"inber": {
			"main":             1_700_000_000_000,
			"hook:abc":         1_700_000_001_000,
			"subagent:def":     1_700_000_002_000,
		},
		"brigid": {
			"main": 1_700_000_003_000,
		},
		"empty-agent": {
			// no main; should not appear
			"hook:zzz": 1_700_000_004_000,
		},
	})

	out := captureStdout(t, func() { emitDiscover(&Config{OpenClawDir: dir}) })

	var sessions []msg.StoredSession
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &sessions); err != nil {
		t.Fatalf("invalid discover JSON: %v\n%s", err, out)
	}
	if len(sessions) != 2 {
		t.Fatalf("len = %d, want 2 (one main per agent that has one); got: %+v", len(sessions), sessions)
	}

	byID := make(map[string]msg.StoredSession, len(sessions))
	for _, s := range sessions {
		byID[s.HarnessSessionID] = s
	}
	for _, want := range []string{"inber:main", "brigid:main"} {
		got, ok := byID[want]
		if !ok {
			t.Errorf("missing HarnessSessionID %q in discover output", want)
			continue
		}
		if got.Harness != msg.HarnessOpenClaw {
			t.Errorf("%s: Harness = %q, want %q", want, got.Harness, msg.HarnessOpenClaw)
		}
		if got.BridgeSessionID != "" {
			t.Errorf("%s: BridgeSessionID = %q, want empty (bridge-server mints the chain head)", want, got.BridgeSessionID)
		}
		agent := strings.TrimSuffix(want, ":main")
		if got.Project != agent {
			t.Errorf("%s: Project = %q, want %q", want, got.Project, agent)
		}
		if got.Prompt != "main" {
			t.Errorf("%s: Prompt = %q, want %q", want, got.Prompt, "main")
		}
		if !strings.HasSuffix(got.Path, ".jsonl") {
			t.Errorf("%s: Path = %q, want .jsonl suffix", want, got.Path)
		}
	}
}

func TestEmitDiscover_UpdatedAtFromSessionsJSON(t *testing.T) {
	const ms int64 = 1_700_123_456_789
	dir := scaffoldOpenClawDir(t, map[string]map[string]int64{
		"inber": {"main": ms},
	})

	out := captureStdout(t, func() { emitDiscover(&Config{OpenClawDir: dir}) })

	var sessions []msg.StoredSession
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &sessions); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(sessions) != 1 {
		t.Fatalf("len = %d, want 1", len(sessions))
	}
	want := time.UnixMilli(ms).UTC()
	if !sessions[0].UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v, want %v (from sessions.json updatedAt)", sessions[0].UpdatedAt, want)
	}
	if !sessions[0].CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v (mirrors UpdatedAt absent btime)", sessions[0].CreatedAt, want)
	}
}

func TestEmitDiscover_EmptyDirYieldsEmptyArray(t *testing.T) {
	dir := t.TempDir() // no agents/ subdir at all

	out := captureStdout(t, func() { emitDiscover(&Config{OpenClawDir: dir}) })

	if got := strings.TrimSpace(out); got != "[]" {
		t.Errorf("empty dir should yield []; got %q", got)
	}
}

func TestEmitDiscover_UnsetDirYieldsEmptyArray(t *testing.T) {
	out := captureStdout(t, func() { emitDiscover(&Config{OpenClawDir: ""}) })

	if got := strings.TrimSpace(out); got != "[]" {
		t.Errorf("empty OpenClawDir should yield []; got %q", got)
	}
}

// captureStdout replaces os.Stdout for the duration of fn and returns
// everything written. Mirrors the helper in llm-bridge-inber/main_test.go.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()

	fn()
	w.Close()
	return <-done
}
