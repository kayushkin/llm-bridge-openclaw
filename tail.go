package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Tailer watches a JSONL session file and emits canonical events via emitEvent.
type Tailer struct {
	path             string
	bridgeSessionID  string
	harnessSessionID string
	agg              *UsageAggregator
}

// NewTailer creates a tailer for the given JSONL session file. Both session
// ids are stamped on every event the tailer emits.
func NewTailer(path, bridgeSessionID, harnessSessionID string, agg *UsageAggregator) *Tailer {
	return &Tailer{
		path:             path,
		bridgeSessionID:  bridgeSessionID,
		harnessSessionID: harnessSessionID,
		agg:              agg,
	}
}

// Run tails the JSONL file from the current end, emitting events for new entries.
// Blocks until ctx is cancelled.
func (t *Tailer) Run(ctx context.Context) {
	f, err := os.Open(t.path)
	if err != nil {
		log.Printf("tail open error for %s: %v", t.harnessSessionID, err)
		return
	}
	defer f.Close()

	// Seek to end — only process new entries.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		log.Printf("tail seek error: %v", err)
		return
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	var publishCount int

	for {
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			events := translateEntry(line, t.bridgeSessionID, t.harnessSessionID, t.agg)
			for _, ev := range events {
				emitEvent(ev)
				publishCount++
			}
		}

		select {
		case <-ctx.Done():
			log.Printf("tailer %s stopped (%d events)", t.harnessSessionID, publishCount)
			return
		case <-time.After(500 * time.Millisecond):
		}

		// Re-create scanner to pick up new data appended to the file.
		scanner = bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	}
}

// --- Session file resolution ---

// sessionsCache caches parsed sessions.json per agent directory, keyed by file modtime.
type sessionsCache struct {
	mu      sync.Mutex
	entries map[string]*sessionsCacheEntry
}

type sessionEntry struct {
	SessionID   string `json:"sessionId"`
	SessionFile string `json:"sessionFile"`
}

type sessionsCacheEntry struct {
	modTime  time.Time
	sessions map[string]sessionEntry
}

var sessCache = &sessionsCache{entries: make(map[string]*sessionsCacheEntry)}

func (c *sessionsCache) load(sessFilePath string) (map[string]sessionEntry, error) {
	info, err := os.Stat(sessFilePath)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	cached, ok := c.entries[sessFilePath]
	c.mu.Unlock()

	if ok && cached.modTime.Equal(info.ModTime()) {
		return cached.sessions, nil
	}

	data, err := os.ReadFile(sessFilePath)
	if err != nil {
		return nil, err
	}
	var sessions map[string]sessionEntry
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.entries[sessFilePath] = &sessionsCacheEntry{modTime: info.ModTime(), sessions: sessions}
	c.mu.Unlock()

	return sessions, nil
}

// findSessionFile finds the JSONL file for a given session name (e.g. "main")
// by looking up the session key in sessions.json.
func findSessionFile(openclawDir, agentID, sessionName string) (string, error) {
	if openclawDir == "" {
		return "", fmt.Errorf("OPENCLAW_DIR not set")
	}

	sessDir := filepath.Join(openclawDir, "agents", agentID, "sessions")
	sessFilePath := filepath.Join(sessDir, "sessions.json")
	sessions, err := sessCache.load(sessFilePath)
	if err != nil {
		return "", fmt.Errorf("load sessions.json: %w", err)
	}
	key := "agent:" + agentID + ":" + sessionName
	entry, ok := sessions[key]
	if !ok {
		return "", fmt.Errorf("session %q not found", key)
	}
	// sessionFile is the physical transcript path — use it when present.
	var path string
	if entry.SessionFile != "" {
		path = entry.SessionFile
	} else {
		path = filepath.Join(sessDir, entry.SessionID+".jsonl")
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("session file %q not found: %w", path, err)
	}
	return path, nil
}

// resolveSessionName looks up a JSONL filename UUID in sessions.json and returns
// the short session name (e.g. "main", "subagent:3a5db604...", "cron:9b27...").
func resolveSessionName(openclawDir, agentID, fileUUID string) (string, bool) {
	if openclawDir == "" {
		return "", false
	}
	sessFilePath := filepath.Join(openclawDir, "agents", agentID, "sessions", "sessions.json")
	sessions, err := sessCache.load(sessFilePath)
	if err != nil {
		return "", false
	}
	prefix := "agent:" + agentID + ":"
	targetFile := fileUUID + ".jsonl"
	for key, entry := range sessions {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if entry.SessionID == fileUUID {
			return strings.TrimPrefix(key, prefix), true
		}
		if entry.SessionFile != "" && filepath.Base(entry.SessionFile) == targetFile {
			return strings.TrimPrefix(key, prefix), true
		}
	}
	return "", false
}

// discoverAllSessions walks the agents directory and returns all known JSONL session files.
func discoverAllSessions(openclawDir string) []discoveredSession {
	var sessions []discoveredSession
	agentsDir := filepath.Join(openclawDir, "agents")
	agents, err := os.ReadDir(agentsDir)
	if err != nil {
		return sessions
	}
	for _, a := range agents {
		if !a.IsDir() {
			continue
		}
		agentName := a.Name()
		sessDir := filepath.Join(agentsDir, agentName, "sessions")
		files, err := os.ReadDir(sessDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			fileUUID := strings.TrimSuffix(f.Name(), ".jsonl")
			sessionName, ok := resolveSessionName(openclawDir, agentName, fileUUID)
			if !ok {
				continue
			}
			sessions = append(sessions, discoveredSession{
				agent:       agentName,
				sessionName: sessionName,
				path:        filepath.Join(sessDir, f.Name()),
			})
		}
	}
	return sessions
}

type discoveredSession struct {
	agent, sessionName, path string
}

// watchNewSessions monitors for new JSONL session files and starts tailers for them.
func watchNewSessions(ctx context.Context, openclawDir string, agg *UsageAggregator) {
	agentsDir := filepath.Join(openclawDir, "agents")

	known := make(map[string]bool)
	var mu sync.Mutex

	scanAll := func() map[string]discoveredSession {
		newFiles := make(map[string]discoveredSession)
		entries, err := os.ReadDir(agentsDir)
		if err != nil {
			return newFiles
		}
		for _, agentEntry := range entries {
			if !agentEntry.IsDir() {
				continue
			}
			agentName := agentEntry.Name()
			sessDir := filepath.Join(agentsDir, agentName, "sessions")
			files, err := os.ReadDir(sessDir)
			if err != nil {
				continue
			}
			for _, f := range files {
				if !strings.HasSuffix(f.Name(), ".jsonl") {
					continue
				}
				fullPath := filepath.Join(sessDir, f.Name())
				mu.Lock()
				isKnown := known[fullPath]
				mu.Unlock()
				if !isKnown {
					fileUUID := strings.TrimSuffix(f.Name(), ".jsonl")
					sessionName, ok := resolveSessionName(openclawDir, agentName, fileUUID)
					if ok {
						newFiles[fullPath] = discoveredSession{
							agent:       agentName,
							sessionName: sessionName,
							path:        fullPath,
						}
					}
				}
			}
		}
		return newFiles
	}

	// Mark existing files as known.
	for path := range scanAll() {
		known[path] = true
	}

	log.Printf("watching %s for new sessions", agentsDir)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("session watcher stopped")
			return
		case <-ticker.C:
			newFiles := scanAll()
			for path, sess := range newFiles {
				mu.Lock()
				known[path] = true
				mu.Unlock()

				// Skip stale files
				info, err := os.Stat(path)
				if err != nil {
					continue
				}
				if time.Since(info.ModTime()) > 30*time.Second {
					continue
				}

				log.Printf("new session detected: %s (agent=%s, session=%s)", filepath.Base(path), sess.agent, sess.sessionName)
				// Auto-discovered tailers run outside any bridge-server-driven
				// session, so BridgeSessionID is empty; HarnessSessionID is
				// the OpenClaw-side session name (e.g. "main").
				t := NewTailer(path, "", sess.sessionName, agg)
				go t.Run(ctx)
			}
		}
	}
}

// listSessions returns all known OpenClaw sessions for a sessions.list response.
func listSessions(openclawDir string) []sessionListEntry {
	var sessions []sessionListEntry
	agentsDir := filepath.Join(openclawDir, "agents")
	filepath.WalkDir(agentsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") || strings.Contains(path, ".deleted") {
			return nil
		}
		rel, _ := filepath.Rel(agentsDir, path)
		parts := strings.Split(rel, string(filepath.Separator))
		agent := ""
		if len(parts) >= 1 {
			agent = parts[0]
		}
		info, _ := d.Info()
		modified := ""
		if info != nil {
			modified = info.ModTime().Format(time.RFC3339)
		}
		sessions = append(sessions, sessionListEntry{
			ID:         strings.TrimSuffix(d.Name(), ".jsonl"),
			Agent:      agent,
			Status:     "idle",
			LastActive: modified,
		})
		return nil
	})
	return sessions
}

type sessionListEntry struct {
	ID         string `json:"id"`
	Agent      string `json:"agent"`
	Status     string `json:"status"`
	LastActive string `json:"last_active,omitempty"`
}
