# Claude Code hooks

If hooks are disabled by policy but MCP is permitted, use the
[Claude Code MCP example](../claude-code-mcp/README.md) instead. It exposes
explicit search/save tools without hooks or automatic transcript capture.

This example connects Claude Code to a running niromem API using Claude Code's
`UserPromptSubmit` and `Stop` hooks:

- Before a prompt is answered, it retrieves a small, relevant memory context.
- After a response, it stores the latest user/assistant text pair as a
  conversation run. niromem processes the turns asynchronously.

The hook is a small Go program and adds **no third-party dependencies**. It
uses the niromem client already in this repository. It does not call Claude,
an embedding API, or any LLM for extraction.

## Run locally

Start a niromem server. For local development, the CLI's default configuration
uses SQLite and development authentication:

```sh
go run ./cmd/niromem serve
```

Build the hook binary from the repository root:

```sh
mkdir -p .claude/hooks
go build -o .claude/hooks/niromem-claude-code ./examples/claude-code
```

Add these hooks to your project's `.claude/settings.local.json` (or merge them
into its existing Claude Code settings):

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "${CLAUDE_PROJECT_DIR}/.claude/hooks/niromem-claude-code",
            "args": ["context"]
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "${CLAUDE_PROJECT_DIR}/.claude/hooks/niromem-claude-code",
            "args": ["record"]
          }
        ]
      }
    ]
  }
}
```

Launch Claude Code from a shell where the optional settings are configured:

```sh
export NIROMEM_URL=http://127.0.0.1:8080
# Optional with development auth; use an API key for a secured server.
export NIROMEM_API_KEY=
# Set these to match the user/agent bound to your API key when using key auth.
export NIROMEM_USER_ID=alex
export NIROMEM_AGENT_ID=claude-code
claude
```

For a secured server, keep the API key in the shell environment or a secret
manager; do not put it in Claude Code settings or commit it. Create a key bound
to the configured user and agent so the hook cannot act as other identities.

The hook waits up to three seconds for the API. If niromem is unavailable or
returns an error, it logs to stderr and lets Claude Code continue. This is a
best-effort workflow integration, not a blocking dependency.

## Data and scope

Each Claude Code session ID is used as the niromem run ID. At `Stop`, the hook
reads the transcript tail and stores only the latest user and assistant text
messages, excluding tool calls and sidechain messages. It does not upload
images, files, tool output, or the full transcript. The normal niromem
conversation pipeline extracts memories from those stored turns.

The example's context is bounded to eight items and 2,000 characters. Search
and conversation history follow the user/agent scope configured on the API
key (or the optional environment variables with development authentication).
Retrieved memory is explicitly labeled as untrusted context for Claude to
verify against the current repository. Use separate identities when you need
project-level isolation.

## Check the example

```sh
go test ./examples/claude-code
```
