# pigo-server

`pigo-server` is an HTTP orchestrator plus an embedded React UI. The **agent
loop and provider API keys stay in this process**. File tools (`read` / `write`
/ `edit` / `grep` / `find`) run against the session workspace on the host.
`bash` runs inside a long-lived bwrap that has **no provider keys** in its
environment. `webfetch` / `websearch` also run on the host so Tavily/Brave keys
never enter the sandbox.

## Requirements

- `bwrap` on `PATH` (package `bubblewrap`) when `bash` is enabled
- unprivileged user namespaces enabled

If `bwrap` is missing and `bash` is in the tool set, `/healthz` reports
`degraded` and sending a message returns `503`. Provider keys are never
injected into the sandbox as a fallback.

## Development

```bash
go run ./cmd/pigo-server
pnpm --dir frontend install
pnpm --dir frontend dev
```

Vite proxies `/api` and `/healthz` to `127.0.0.1:8080`.

## Production build

```bash
pnpm --dir frontend install --frozen-lockfile
pnpm --dir frontend build
go build -o pigo-server ./cmd/pigo-server
```

The Vite build is written to `web/dist` and compiled into `pigo-server` with
`go:embed`.

## Run

```bash
PIGO_SERVER_TOKEN='change-me' \
OPENROUTER_API_KEY='your-provider-key' \
./pigo-server -listen 0.0.0.0:8080 -tools all
```

| Flag / env | Meaning |
|---|---|
| `-listen` | HTTP address (default `127.0.0.1:8080`). Non-loopback requires `PIGO_SERVER_TOKEN`. |
| `-data` | Session root (default `$PIGO_SERVER_DATA` or `~/.pigo-server`) |
| `-tools` | Empty = `--no-tools`. `all` = full builtin set. Comma-list = allowlist. |
| `-skills` | Bind the host skills directory into the sandbox (off by default). |
| `-model` / `-thinking` / `-provider` | Defaults for new sessions |
| `-idle` | Stop an idle sandbox process after this duration (default `30m`). `0` never expires the process. Disk is always kept. No hard lifetime cap. |
| `PIGO_SERVER_BWRAP` | Path to `bwrap` |

Provider API keys (`OPENROUTER_API_KEY`, `ANTHROPIC_API_KEY`, `TAVILY_API_KEY`,
…) stay in the orchestrator process environment. They are **not** `--setenv`'d
into bwrap. The host `HOME` is never bind-mounted.

Each session lives at:

```
<data>/sessions/<api-id>/
  workspace/     → /workspace in bwrap (agent files)
  home/          → /home/pigo (PIGO_HOME, session JSONL)
  run/           → supervisor IPC (fifo / job scripts)
  meta.json
```

The bwrap process stays up between `bash` calls (same pid namespace and `/tmp`).
After `-idle` with no traffic the process is killed; workspace files and the
host-side transcript under `transcript/` survive. `DELETE /api/sessions/{id}`
removes disk state. Server shutdown stops processes and leaves directories in
place.

Conversation transcript is stored on the host (`transcript/chat.jsonl`), not
inside the sandbox. The model is invoked only from `pigo-server`.

## HTTP API

- `GET /healthz`
- `POST /api/sessions`
- `GET /api/sessions/{id}`
- `PATCH /api/sessions/{id}` with `{ "model": "...", "thinking": "..." }`
- `DELETE /api/sessions/{id}`
- `GET /api/models`
- `GET /api/sessions/{id}/commands`
- `POST /api/sessions/{id}/messages`
- `POST /api/sessions/{id}/messages?stream=true` (NDJSON: `delta`, `tool`, `done`, `error`)
- `GET /api/sessions/{id}/files?path=`
- `GET /api/sessions/{id}/files/raw?path=`

The Web surface implements `/model`, `/models`, `/think`, `/effect`, and
`/help`. TUI-only commands stay listed but unavailable.

## Key isolation

- LLM and search keys: orchestrator env only.
- `bash` in bwrap: `--clearenv`, no `*_API_KEY`.
- File tools: host process, rooted at the session workspace (cannot read host `$HOME`).
- Project `.env` files in the workspace can still be read by the model; that is
  not a platform-key leak.

## Isolation (v1)

bwrap unshares pid/ipc/uts, uses a private `/tmp`, bind-mounts only the session
workspace + home, and `--clearenv`s before injecting credentials. Network is
still the host network (needed for model APIs and `webfetch`). That is not a
multi-tenant security boundary; it is filesystem/PID isolation so sessions do
not share a cwd or `~/.pigo`. There is no wall-clock cap on how long a busy
sandbox may live.
