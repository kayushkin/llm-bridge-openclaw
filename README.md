# llm-bridge-openclaw

Harness bridge for [OpenClaw](https://github.com/kayushkin/openclaw), translating between the llm-bridge subprocess protocol (NDJSON JSON-RPC on stdin/stdout) and OpenClaw's OpenAI-compatible REST API plus on-disk JSONL session transcripts.

## Architecture

OpenClaw exposes two surfaces and this bridge consumes both:

```
llm-bridge (stdin JSON-RPC)
    ↓
llm-bridge-openclaw
    ├── POST /v1/chat/completions (SSE) ──→ OpenClaw gateway (:18789)
    │     (sends user input, drains the SSE stream to keep the request open)
    │
    └── tail $OPENCLAW_DIR/agents/<agent>/sessions/<id>.jsonl
          (translates JSONL entries into canonical msg.Event)
    ↓
stdout NDJSON (canonical msg.Event)
```

The split exists because the SSE stream from `/v1/chat/completions` only confirms the request was accepted — the actual assistant turns, thinking blocks, tool calls and final usage are written by OpenClaw to its on-disk JSONL transcripts. The bridge tails those files and converts each entry into the canonical `msg.Event` shape.

## Build

This module uses a local `replace` directive for `github.com/kayushkin/llm-bridge`, so both repos must be checked out side-by-side:

```
repos/
├── llm-bridge/
└── llm-bridge-openclaw/
```

Then:

```bash
go build -o llm-bridge-openclaw
```

> **Pre-publish note:** the `replace github.com/kayushkin/llm-bridge => ../llm-bridge` line in `go.mod` must be removed (or moved to a `go.work` file) before tagging a release, otherwise downstream `go get github.com/kayushkin/llm-bridge-openclaw@v…` will fail.

## Usage

```bash
# Normal mode — reads JSON-RPC requests from stdin, emits NDJSON events to stdout.
./llm-bridge-openclaw

# Print version.
./llm-bridge-openclaw -version

# Print the sessions this bridge can resume, as a JSON array of msg.StoredSession.
./llm-bridge-openclaw -discover
```

`-import-history` is not implemented: it prints a message on stderr and exits 2.

Send a JSON-RPC request to start a session:

```json
{"method":"start","params":{"session_id":"sess-1","agent_id":"main","prompt":"Hello!"}}
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OPENCLAW_URL` | `http://127.0.0.1:18789` | OpenClaw gateway base URL |
| `OPENCLAW_DIR` | `~/.openclaw` | Path to OpenClaw's storage directory (the parent of `agents/`). Every event with assistant output comes from the transcripts under it. If it points at the wrong place, `start` and `message` still reach OpenClaw, but the bridge surfaces no assistant output. |
| `OPENCLAW_TOKEN` | — | Optional bearer token sent as `Authorization: Bearer …` |

The bridge also sends OpenClaw-specific request headers automatically:

- `x-openclaw-scopes: operator.write`
- `x-openclaw-agent-id: <agent_id>`
- `x-openclaw-session-key: agent:<agent_id>:main`

## JSON-RPC Methods

| Method | Description |
|--------|-------------|
| `start` | Initialize the session, start the JSONL tailer, and forward `prompt` as the first user message. Params: `bridge_session_id`, `harness_session_id`, `session_id` (deprecated; read only as the bridge session id), `agent_id` (default `main`), `prompt`, `display_name`, `resume`, `fork` |
| `message` | Send a follow-up message. Params: `content` |
| `compact` | Refused with an `error` event, code `UNSUPPORTED`: the chat-completions endpoint has no compaction operation, and the bridge has no other way into OpenClaw |
| `resume` | Restart the JSONL tailer if not running |

## Canonical Events Emitted

`stream`, `thinking`, `tool_call`, `tool_result`, `result`, `error`, `system`, `session_state`

OpenClaw's JSONL `content` blocks translate as follows:

| OpenClaw block | Canonical event |
|----------------|-----------------|
| `thinking` | `thinking` + `stream` (DeltaThinking) |
| `text` (assistant) | `stream` (DeltaText) |
| `toolCall` | `tool_call` |
| `text` (toolResult role) | `tool_result` |
| message with `stopReason=stop` | final `result` (with aggregated token usage and cost) + `session_state(idle)` |

Outbound text matching well-known no-op markers (`HEARTBEAT_OK`, `NO_REPLY`, `API CALL`, `TOOL CALL`) is forwarded with `Hidden: true` on the stream delta.

## Session File Resolution

When `OPENCLAW_DIR` is set, the bridge opens:

```
$OPENCLAW_DIR/agents/<agent_id>/sessions/sessions.json
```

…to look up the session key `agent:<agent_id>:main` and resolve the physical JSONL transcript path (either `sessionFile` or `<sessionId>.jsonl` in the same directory). The resolved file is tailed from the current end — only entries appended after the bridge starts are translated.

`sessions.json` is parsed once and cached per-modtime.

## Token Usage and Cost

Per-turn token usage is aggregated across all assistant messages in the turn (input, output, cache-read, cache-write, total). When OpenClaw reports per-call cost in the JSONL `usage.cost` block, it is forwarded on the final `result` event as `msg.Cost` (USD).

## Testing

```bash
go vet ./...
go test ./...
scripts/e2e-smoke.sh         # boots, discovers, answers
scripts/interrupt-smoke.sh   # drives the real binary through the interrupt contract
```

`interrupt-smoke.sh` signals the built process the way bridge-server does and
asserts the session survives it. It needs no OpenClaw: the bridge's only
outbound channel is `POST /v1/chat/completions`, so it stubs that endpoint with
a stream that never ends, which is the shape of a long turn.

## Known Gaps

- **No `interrupt` method** — but interrupting works. bridge-server's Stop is a
  SIGINT, not a JSON-RPC call (`internal/harness/manager.go` Stop →
  `proc.Interrupt`), and `SendJSONRPC` has no caller anywhere in that repo. So
  the signal handler is the interrupt contract: SIGINT cancels the in-flight
  turn's POST and its SSE drain, the harness and the JSONL tailer stay up, and
  the next message continues the same session. SIGTERM still ends the process.
  What this cannot promise is that OpenClaw stops generating — the cancel is a
  client disconnect, and whatever OpenClaw keeps producing still arrives through
  the tailer.
- **No system prompt**: `start.system_prompt` is not forwarded — OpenClaw's agent persona/system message is configured server-side.
- **No `set_model` / `config`**: model selection is determined by the OpenClaw gateway; the request always uses `model: "openclaw"`.
- **No `discover` method**: discovery is the `-discover` flag, not a JSON-RPC call, and it lists only each agent's `main` session, because that is the only one the bridge can resume. `listSessions` and `watchNewSessions` in `tail.go` have no callers.
- **No `fork` translation**: OpenClaw has no session-cloning primitive, so a
  `start` carrying `fork` is refused with `FORK_UNSUPPORTED` rather than
  silently starting a fresh chain. OpenClaw branches are managed via its own
  dashboard.
- **Single hardcoded session name**: the bridge tails the `main` session for an agent; multi-session-per-agent is not exposed.

## Part of the llm-bridge ecosystem

- [llm-bridge](https://github.com/kayushkin/llm-bridge) — canonical message types (`msg/`) and bridge interfaces.
- [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server) — central HTTP gateway and session server that launches harness binaries like this one.
- [llm-bridge-claudecode](https://github.com/kayushkin/llm-bridge-claudecode), [llm-bridge-hermes](https://github.com/kayushkin/llm-bridge-hermes), [llm-bridge-kilocode](https://github.com/kayushkin/llm-bridge-kilocode) — sibling harness bridges for other agents.

## License

Apache 2.0. See [LICENSE](./LICENSE).
