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
```

Send a JSON-RPC request to start a session:

```json
{"method":"start","params":{"session_id":"sess-1","agent_id":"main","prompt":"Hello!"}}
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OPENCLAW_URL` | `http://127.0.0.1:18789` | OpenClaw gateway base URL |
| `OPENCLAW_DIR` | — | Path to OpenClaw's storage directory (the parent of `agents/`). Required for event emission — without it, `start` and `message` will reach OpenClaw, but the bridge will not surface any assistant output. |
| `OPENCLAW_TOKEN` | — | Optional bearer token sent as `Authorization: Bearer …` |

The bridge also sends OpenClaw-specific request headers automatically:

- `x-openclaw-scopes: operator.write`
- `x-openclaw-agent-id: <agent_id>`
- `x-openclaw-session-key: agent:<agent_id>:main`

## JSON-RPC Methods

| Method | Description |
|--------|-------------|
| `start` | Initialize the session, start the JSONL tailer (if `OPENCLAW_DIR` is set), and forward `prompt` as the first user message. Params: `session_id`, `agent_id` (default `main`), `prompt`, `display_name`, `resume`, `fork` |
| `message` | Send a follow-up message. Params: `content` |
| `compact` | Acknowledged with a `system` event; OpenClaw manages compaction internally |
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
go build ./...
```

There are no unit tests in this module yet. The harness is exercised via end-to-end runs against a real OpenClaw gateway plus the bridge-ui Conformance page.

## Known Gaps

- **No `interrupt` method**: in-flight turns cannot be cancelled via JSON-RPC; the SSE request runs to completion (or its 10-minute timeout).
- **No system prompt**: `start.system_prompt` is not forwarded — OpenClaw's agent persona/system message is configured server-side.
- **No `set_model` / `config`**: model selection is determined by the OpenClaw gateway; the request always uses `model: "openclaw"`.
- **No `discover`**: the source contains `discoverAllSessions` and `listSessions` helpers in `tail.go`, but they are not wired into the JSON-RPC dispatch.
- **No `fork` translation**: the `fork` field is parsed but not acted on; OpenClaw branches are managed via its own dashboard.
- **Single hardcoded session name**: the bridge tails the `main` session for an agent; multi-session-per-agent is not exposed.

## Part of the llm-bridge ecosystem

- [llm-bridge](https://github.com/kayushkin/llm-bridge) — canonical message types (`msg/`) and bridge interfaces.
- [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server) — central HTTP gateway and session server that launches harness binaries like this one.
- [llm-bridge-claudecode](https://github.com/kayushkin/llm-bridge-claudecode), [llm-bridge-hermes](https://github.com/kayushkin/llm-bridge-hermes), [llm-bridge-kilocode](https://github.com/kayushkin/llm-bridge-kilocode) — sibling harness bridges for other agents.

## License

Apache 2.0. See [LICENSE](./LICENSE).
