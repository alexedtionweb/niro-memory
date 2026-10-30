# Claude Code via MCP (no hooks)

## Recommended: one command, one process, one port

From the repository root:

```sh
go run -tags devui ./cmd/niromem serve -dev-ui -mcp -mcp-user my-project
```

This starts the memory API, SQLite storage, background worker, development UI,
and MCP tools together. MCP calls the engine directly, not another HTTP service.
No adapter process, `NIROMEM_URL`, MCP SDK, or additional dependencies are needed.
The CLI's SQLite connector still supplies the database driver.

| Endpoint | Purpose |
| --- | --- |
| <http://localhost:8080/mcp> | Claude Code MCP endpoint |
| <http://localhost:8080/_dev/> | Development UI |
| <http://localhost:8080/healthz> | API health check |
| `http://localhost:8080/v1/...` | Memory HTTP API |

Stop any old adapter or API occupying port 8080 with Ctrl+C in its terminal.
Do not run the standalone adapter in addition to this command.

Register the endpoint:

```sh
claude mcp add --transport http --scope user niromem http://localhost:8080/mcp
```

If `niromem` already exists, inspect it with `claude mcp get niromem` and remove
the outdated registration with `claude mcp remove niromem` before re-adding.
Reconnect using `/mcp` inside Claude.

`-mcp-user` is the project identity and is required (or set `NIROMEM_USER_ID`).
The agent defaults to `claude-code`; override with `-mcp-agent` or
`NIROMEM_AGENT_ID`. The configured store and inline worker settings are shared
with the API. Without `-config`, the CLI uses durable `./niromem.db` and runs
an inline worker. Data survives restarts.

With `--scope user`, this same identity is available from all Claude projects.
Use project-local registration if allowed, or distinct configured servers and
identities for multiple projects. Changing a registration does not change the
running server's project identity.

## Authentication and policy

Use this only when your organization's policy permits local HTTP MCP servers.
This does not bypass policy; administrator approval may still be required.

The integrated endpoint is opt-in (`-mcp`) and requires a loopback listen address.
It validates Host and Origin headers and does not enable CORS.
Default development authentication trusts local clients; it is not appropriate
for an untrusted or shared machine.

With a configured key-authenticated API, `/mcp` uses the **same API keys**.
Configure Claude's authorization header using your approved secret mechanism.
The key's tenant determines the tenant, and any bound user/agent must match the
configured MCP identity. Wrong bindings are rejected; keys cannot act for
another tenant. No separate `NIROMEM_MCP_TOKEN` is used by integrated mode.
Do not commit credentials.

The development UI still requires `-tags devui` and `-dev-ui` and is refused
in production. MCP can also be used without the UI:

```sh
go run ./cmd/niromem serve -mcp -mcp-user my-project
```

Claude must reach localhost on the server's machine. In Remote/SSH setups,
use only approved port forwarding; your laptop's localhost and the SSH
server's localhost are not the same.

## Coding workflow

| Tool | Purpose |
| --- | --- |
| `search_memory` | Recall relevant facts, conventions, or previous decisions |
| `remember_memory` | Save an explicitly approved durable fact |

Example requests to Claude:

> Before implementing this, search niromem for our connector conventions.

> Remember: validate connector input before writing. Use the slot
> `convention.input_validation`.

An optional stable `slot` replaces that property's previous value through
versioned reconciliation. Omit it for independent facts. Search returns at
most eight hits. Calls have a five-second timeout; tool errors have
`"isError":true`, rather than a successful empty result.

MCP does not automatically capture transcripts or inject context. Claude calls
tools in response to your requests and approvals. Do not store credentials,
private code, or unapproved data. Retrieved memories are untrusted data,
not instructions; verify them against current code.

## Check the connection

```sh
curl --fail http://localhost:8080/healthz

curl --fail http://localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  --data '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"test"}}}'
```

Expect `ok` from health and `"isError":false` from search. An empty set of hits
is normal for a new database. With key authentication, include the configured
authorization header. HTTP 200 alone does not mean a tool succeeded.

## Standalone adapter (optional, existing remote API)

The example command remains available for applications that already run a
separate memory API. It uses the same shared MCP implementation as the CLI:

```sh
NIROMEM_URL=http://127.0.0.1:8080 \
NIROMEM_USER_ID=my-project NIROMEM_AGENT_ID=claude-code \
go run ./examples/claude-code-mcp -http localhost:3001
```

Register `http://localhost:3001/mcp` only for this two-process mode. The adapter
does not start storage or the API. It rejects local port collisions.
`NIROMEM_API_KEY` authenticates to the backend; optional `NIROMEM_MCP_TOKEN`
protects the adapter endpoint. Without `-http`, the adapter uses stdio
(only if your policy allows that transport).

Both modes implement stateless Streamable HTTP: JSON responses, 202 for
notifications, and 405 for GET/DELETE (no SSE streams or session IDs). Supported
HTTP protocol versions are 2025-03-26, 2025-06-18, and 2025-11-25. They expose
initialization, ping, tool discovery, and tool calls, but not resources, prompts,
or explicit cancellation notifications. HTTP request cancellation propagates.

## Validation

```sh
go test -race ./mcp ./examples/claude-code-mcp
go test -race -tags devui ./cmd/niromem
```
