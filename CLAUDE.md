# read-once-hook — Project Guide for Claude Code

## Build & Install

```bash
go build -o read-once .
./read-once install
```

The compiled binary is self-contained (statically linked). The repo also ships a pre-built
`read-once` binary for quick-start installs; rebuild to pick up local changes.

## Test

No automated tests exist yet. Use the built-in dry-run diagnostic:

```bash
./read-once verify
```

This installs a temp session, runs two hook invocations, and checks the JSON output format.

## Lint / Format

```bash
gofmt -l .          # list files needing formatting
gofmt -w main.go    # apply formatting
go vet ./...        # static analysis
```

The project must pass both `gofmt` (zero diff) and `go vet` before any commit.

## Architecture

Single-file Go program (`main.go`, ~2300 LOC). No sub-packages.

### Supported clients

| Client | Hook type | Config file | Matcher |
|---|---|---|---|
| `claude` (Claude Code) | `PreToolUse` JSON hook | `~/.claude/settings.json` | `"Read"` |
| `codex` | `PreToolUse` JSON hook | `~/.codex/hooks.json` | `"Bash"` |
| `opencode` | JS plugin (`tool.execute.before`) | `~/.config/opencode/plugins/read-once.js` | tool === "read" or "bash" |

Client detection order: `READ_ONCE_CLIENT` env → executable path heuristic → working directory
heuristic → default `claude`.

### Hook protocol (Claude Code / Codex)

The binary reads a JSON object from stdin and writes a JSON object to stdout.

**Input fields consumed:**
- `tool_name` / `toolName` / `tool` / `name` — tool being called
- `tool_input.file_path` / `tool_input.path` — file being read (for `Read` tool)
- `tool_input.command` — shell command (for `Bash` / Codex)
- `session_id` / `conversation_id` / `thread_id` — session identifier
- `cwd` — working directory (used to resolve relative paths in Bash commands)

**Output — deny mode:**
```json
{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"<reason>"}}
```

**Output — warn mode (Claude Code only):**
```json
{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"<advisory>"}}
```

**Output — allow mode or first read:** no output, exit 0.

**Codex note:** Codex rejects `permissionDecision:"allow"`. In warn mode, the binary emits
nothing for Codex (silent pass-through). Deny mode still emits correctly.

### Hook protocol (OpenCode)

OpenCode uses a JavaScript plugin (`renderOpenCodePlugin`) loaded from
`~/.config/opencode/plugins/read-once.js`. The plugin calls the binary as a subprocess and
handles the output.

**Critical constraint:** OpenCode's `tool.execute.before` hook returns `Promise<void>`.
The Plugin.trigger dispatcher never captures the return value — only `throw` affects execution.
There is **no advisory channel for allow decisions**. Warn mode emits allow+reason JSON from the
binary, but the plugin discards it silently. Effective modes for OpenCode:
- `deny` — blocks the re-read (the only mode that saves tokens for OpenCode)
- `warn` / `allow` — identical: silent pass-through, no token savings

The optimized OpenCode install therefore defaults to `READ_ONCE_MODE_UNCHANGED=deny` and
`READ_ONCE_MODE_CHANGED=allow`.

### Session cache

- Location: `$READ_ONCE_HOME/session-<session_hash>.jsonl` (one file per session)
- Format: append-only JSONL, one `cacheEntry` per line
- Lookup: full sequential scan (`readLastCacheEntry`) — O(n) in session length. Bounded in
  practice by TTL (default 20 min) and `READ_ONCE_MAX_BYTES`. Acceptable for typical sessions
  (<500 unique file reads). Each hook invocation is a fresh process, so no cheaper alternative
  exists without a daemon.
- Concurrency: protected by a spin-lock file (`<path>.lock`) with 2-second timeout. On timeout,
  the write is **skipped** (not retried without a lock). Stats may under-count under heavy
  parallel tool-call load; this is preferred over corrupted JSONL.

### Stats

- Location: `$READ_ONCE_HOME/stats.jsonl`
- Events: `miss` (first read), `hit` (cached, unchanged), `diff` (changed, diff sent),
  `changed` (changed, full re-read allowed), `expired` (TTL expired, re-read allowed)

### Known limitations

1. **UTF-16 files silently skipped**: `isLikelyBinary` triggers on any null byte. UTF-16 LE/BE
   encoded text files are misclassified as binary. See comment in `isLikelyBinary`.
2. **O(n) session cache scan**: see `readLastCacheEntry` comment.
3. **Lock timeout drops writes**: see `appendJSONLine` comment.
4. **OpenCode warn mode = no-op**: see Hook protocol (OpenCode) section above.
5. **Static bypass list**: `shouldBypassPath` has a hardcoded list of path segments to skip
   (`.git/`, `node_modules/`, etc.). New patterns must be added manually or via
   `READ_ONCE_EXCLUDE`.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `READ_ONCE_MODE` | `warn` | `warn` / `deny` / `allow` |
| `READ_ONCE_MODE_UNCHANGED` | inherits `READ_ONCE_MODE` | Mode for unchanged-file re-reads |
| `READ_ONCE_MODE_CHANGED` | inherits `READ_ONCE_MODE` | Mode for changed-file re-reads |
| `READ_ONCE_TTL` | `300` | Cache TTL in seconds |
| `READ_ONCE_DIFF` | `0` | `1` = send diff for changed files instead of full re-read |
| `READ_ONCE_DIFF_MAX` | `40` | Max diff lines before falling back to summary |
| `READ_ONCE_HASH` | `0` | `1` = validate unchanged reads by content hash |
| `READ_ONCE_HASH_ALGO` | `xxhash` | `xxhash` or `sha256` |
| `READ_ONCE_MAX_BYTES` | `1048576` | Skip files larger than this (bytes) |
| `READ_ONCE_AUTO_ALLOW` | `2` | Auto-allow re-read on the Nth consecutive blocked attempt |
| `READ_ONCE_DECAY` | `60` | Time window (seconds) to consider attempts consecutive |
| `READ_ONCE_INCLUDE` | `` | Comma-separated glob/`re:regex` include patterns |
| `READ_ONCE_EXCLUDE` | `` | Comma-separated glob/`re:regex` exclude patterns |
| `READ_ONCE_DISABLED` | `0` | `1` = disable hook entirely |
| `READ_ONCE_DEBUG` | `0` | `1` = write skip reasons to `$READ_ONCE_HOME/debug.log` |
| `READ_ONCE_CLIENT` | auto-detected | `claude` / `codex` / `opencode` |
| `READ_ONCE_HOME` | client-specific | Override cache/binary directory |
| `READ_ONCE_SETTINGS_FILE` | client-specific | Override settings file path |
| `READ_ONCE_HOOK_COMMAND` | client-specific | Override hook command string |
