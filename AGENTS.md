# About llm-bridge-openclaw

## What it owns

The harness that lets llm-bridge-server run the OpenClaw agent. It is a Go program, `package main`. llm-bridge-server starts one copy per session from `~/bin/llm-bridge-openclaw`. It reads requests as JSON lines on stdin and writes `msg.Event` JSON lines on stdout. Its harness id is `openclaw` (`msg.HarnessOpenClaw`). It owns no data and listens on no port. OpenClaw owns the sessions and the transcripts; this program only reads them.

⚠️ **It is not a WebSocket client.** An older description said so, and a todo once recorded a design blocker that did not exist because of it.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Two channels: one to send, one to read

A turn goes out over HTTP and comes back through a file.

- **Send** (`client.go`): `POST $OPENCLAW_URL/v1/chat/completions`, the OpenAI-compatible endpoint, with `model: "openclaw"`, `stream: true` and one user message. The headers `x-openclaw-agent-id` and `x-openclaw-session-key: agent:<agent_id>:main` pick the session. The SSE reply is read to `[DONE]` and thrown away: it only shows the request is still open.
- **Read** (`tail.go`, `translate.go`): OpenClaw writes each assistant message, thinking block, tool call, tool result and usage figure to a JSONL transcript. The harness finds the file through `$OPENCLAW_DIR/agents/<agent_id>/sessions/sessions.json`, seeks to the end, polls every 500 ms, and turns each new line into `msg.Event`s. A message with `stopReason=stop` ends the turn: one `result` event with the summed token usage and cost, then the state `idle`.

So a fault in `client.go` shows as a turn that never starts, and a fault in `tail.go` or `translate.go` shows as a turn that runs and says nothing. Only lines written after the tailer starts are seen; nothing replays history, and `-import-history` exits 2 to say so.

## Requests it answers, and the ones it refuses

`start`, `message`, `compact` and `resume` (`handler.go`).

- `start` takes `bridge_session_id`, `harness_session_id`, `agent_id` (default `main`), `prompt`, `resume` and `fork`. `session_id` is the old name and is read only as the bridge session id.
- **The harness session id stays empty unless the caller sends one.** OpenClaw never reports its own id over this endpoint. Do not fill it from the bridge session id: llm-bridge-server treats equal ids as a contract breach, discards the value and logs on every event.
- `fork` is refused with the error `FORK_UNSUPPORTED`, and `compact` with `UNSUPPORTED`. OpenClaw offers neither over this endpoint. Both used to be accepted and do nothing; keep them refusing.
- **Stop is a SIGINT, not a request.** llm-bridge-server sends the signal and has no interrupt method. `watchSignals` in `main.go` cancels the turn in flight, emits `INTERRUPTED`, and leaves the process, the tailer and the session running. It is a loop so that the second Stop of a session works too. SIGTERM ends the process. Cancelling only closes our HTTP request; whether OpenClaw stops generating is OpenClaw's business, and whatever it still writes arrives through the tailer.
- `-discover` prints the sessions that can be resumed as a JSON array of `msg.StoredSession`: one per agent, id `<agent>:main`. It prints `[]` when the directory is missing or unreadable.

## Only the main session of an agent

`sendMessage` and `startTailer` both hardcode the session name `main`, and `-discover` lists only `main` sessions to match. Subagent, cron and hook sessions exist on disk and cannot be driven from here. Widening this means changing all three places together. `listSessions` and `watchNewSessions` in `tail.go` are an earlier attempt and have no callers.

# Access and operations

## Configuration

Three environment variables, read once in `config.go`: `OPENCLAW_URL` (default `http://127.0.0.1:18789`), `OPENCLAW_DIR` (default `~/.openclaw`, the parent of `agents/`) and `OPENCLAW_TOKEN` (optional, sent as a bearer token). On this host `~/.openclaw` exists and the `openclaw` CLI is installed, but the gateway is not always listening; a session whose gateway is down fails its first send with `SEND_ERROR`.

## Build, test and deploy

```sh
go vet ./... && go test ./...          # 35 tests, under a second
scripts/e2e-smoke.sh                   # boots, discovers and answers
scripts/interrupt-smoke.sh             # SIGINT cancels the turn and the session lives
python3 scripts/sabotage-truncation.py # breaks the truncation fix in place and checks which tests notice
./deploy.sh                            # from a clean, pushed main only
```

None of these needs OpenClaw running: the smokes stub the endpoint. `go.mod` replaces `github.com/kayushkin/llm-bridge` with `../llm-bridge`, so that clone must sit beside this one, and the deploy gate checks it too.

`deploy.sh` runs the tests and both smokes, refuses a binary with no `vcs.revision`, copies the binary to `~/bin/llm-bridge-openclaw`, and hands it to `llm-bridge-runner/scripts/publish-harness-wrapper.sh` so runner machines download the same build from the bridge. There is no service to restart: the next session llm-bridge-server starts gets the new binary, and sessions already running keep the old one. `-version` prints `0.1.0` and names no commit; use `go version -m ~/bin/llm-bridge-openclaw` or `deploy-gate status` to see what is live.

A build made inside a worktree carries no `vcs.revision` (measured 2026-11-08), which `deploy.sh` refuses, so deploy from the main clone after the merge. The whole suite, including `TestHarnessIdentityIsSelfConsistent`, passes in a worktree since da9d9a0.

# Working in this repo

## Text this harness cuts or hides

Two places shorten text, both through `truncateAtRuneBoundaryWithEllipsis` in `client.go`: a tool result's `Output` is cut at 500 bytes (`translate.go`), and an HTTP error body at 200 bytes. The full line is still on the event's `Raw` field. The cut never splits a UTF-8 character, the `...` marker sits outside the budget, and `truncate_test.go` pins both numbers. The host rule is that a layer passes data through unchanged, so treat the 500-byte cut as a debt, not a pattern: do not add another cut, and if you change a budget, change its test and run the sabotage scorer.

`isHiddenOutbound` marks assistant text that is exactly `HEARTBEAT_OK`, `NO_REPLY`, `API CALL` or `TOOL CALL` (or the underscore forms) as `Hidden`. It still sends it.

The sabotage scorer edits source files in place and puts them back. It refuses to run on a dirty tree or while another run holds the tree (`scripts/tree_hold.py`, a vendored copy of a script other repos carry too; keep the copies identical).
