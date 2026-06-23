# read-once-hook — Project Guide

## Build & Install

```bash
go build -o read-once .
./read-once install
```

To enable the `readOnceClearCache` custom tool in Claude Code, run:

```bash
claude mcp add read-once-tools -- ~/.claude/read-once/read-once mcp
```

The compiled binary is self-contained (statically linked). The repo also ships a pre-built
`read-once` binary for quick-start installs; rebuild to pick up local changes.

## Test

```bash
go test ./...
./read-once verify       # dry-run hook invocation test
```

## Lint / Format

```bash
gofmt -l .               # list files needing formatting
go vet ./...             # static analysis
golangci-lint run ./...  # full lint (must pass 0 issues)
```

The project must pass `gofmt`, `go vet`, and `golangci-lint` before any commit.

## Architecture

Multi-file Go program. No sub-packages.

| File | Purpose |
|---|---|
| `main.go` | Entry point, CLI dispatch, config loading |
| `hook.go` | Core hook logic (stdin JSON → deny/warn/allow) |
| `cache.go` | JSONL session cache, file locks, cleanup |
| `commands.go` | `install`, `uninstall`, `stats`, `verify`, `clear`, etc. |
| `clients.go` | Claude/Codex/OpenCode client-specific install logic |
| `filter.go` | Include/exclude policy, bypass paths |
| `bash.go` | Bash command parser (extracts file reads from `cat`, `head`, etc.) |
| `diff.go` | Unified diff and diff summary for changed files |
| `util.go` | Hash functions, adaptive TTL, helpers |
| `templates/` | Embedded templates for OpenCode plugin, pi extension |

### Supported clients

| Client | Hook type | Config file | Matcher |
|---|---|---|---|
| `claude` (Claude Code) | `PreToolUse` JSON hook | `~/.claude/settings.json` | `"Read"` |
| `codex` | `PreToolUse` JSON hook | `~/.codex/hooks.json` | `"Bash"` |
| `opencode` | JS plugin (`tool.execute.before`) | `~/.config/opencode/plugins/read-once.js` | tool === "read" or "bash" |
| `pi` | TypeScript extension (`tool_call` event) | `~/.pi/agent/extensions/read-once.ts` | toolName === "read" or "bash" |

Client detection order: `READ_ONCE_CLIENT` env → executable path heuristic → working directory
heuristic → default `claude`.

### Pi extension

The pi extension (`templates/pi-extension.ts`) is a standalone TypeScript file — no Go binary
needed. Install by copying to `~/.pi/agent/extensions/read-once.ts`. It auto-discovers and
loads on next `pi` startup or `/reload`.

Key differences from the Go binary:

- **In-memory cache** — per-session, no JSONL files on disk
- **warn mode blocks** — pi has no advisory-only channel; warn and deny both block with reason
- **Hash uses sha256** — Node.js built-in crypto, no xxhash available
- **Custom tools** — `readOnceClearCache` (clear cache) and `readOnceStats` (session stats)
- **Range-aware** — cache key is `path:offset:limit` for ranged reads; base reads share just `path`

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
  practice by TTL and `READ_ONCE_MAX_BYTES`. Acceptable for typical sessions
  (<500 unique file reads). Each hook invocation is a fresh process, so no cheaper alternative
  exists without a daemon.
- Concurrency: protected by a spin-lock file (`<path>.lock`) with 2-second timeout. On timeout,
  the write is **skipped** (not retried without a lock). Stats may under-count under heavy
  parallel tool-call load; this is preferred than corrupted JSONL.

### Cache behavior

**Sliding TTL:** The TTL clock resets on each cache hit. A file read at T=0 with 5min TTL,
re-read at T=3min → new expiry at T=8min. Active sessions rarely expire entries.

**Adaptive TTL:** TTL grows with session duration via `computeAdaptiveTTL()`:

- 0 min → 1x base TTL (default 5min)
- 10 min → 2x (10min)
- 20+ min → 3x (15min, capped)

Combined with sliding TTL, long sessions effectively never expire cached files.

**Hash validation:** Default on (`READ_ONCE_HASH=1`). On cache hit, re-stats the file and
compares content hash (xxhash for Go, sha256 for pi). Catches `touch`-only changes that
mtime alone misses. Fast enough to be default — xxhash hashes a 5KB file in ~3μs.

**Auto-allow:** After `READ_ONCE_AUTO_ALLOW` (default 2) consecutive blocked attempts within
`READ_ONCE_DECAY` seconds (default 60), the read is allowed through. Message shows
"Attempt 1/2 before auto-allow" so the model knows it's coming. Escapes deadlock when the
model doesn't know about `readOnceClearCache`.

**Range-aware cache:** Reads with `offset` parameter use cache key `path:offset:limit`. Reads
without `offset` (even with `limit`) use just `path`. This means:

- `read(path)` and `read(path, limit=100)` share one cache entry
- `read(path, offset=100, limit=50)` gets its own entry
- `readOnceClearCache` clears base + all ranged variants for a path

### Stats

- Location: `$READ_ONCE_HOME/stats.jsonl`
- Events: `miss` (first read), `hit` (cached, unchanged), `diff` (changed, diff sent),
  `changed` (changed, full re-read allowed), `expired` (TTL expired, re-read allowed),
  `auto_allow` (auto-allowed after N blocks)
- `./read-once stats` — overall summary
- `./read-once stats --session <prefix>` — per-session breakdown (prefix match)

### Known limitations

1. **UTF-16 files silently skipped**: `isLikelyBinary` triggers on any null byte. UTF-16 LE/BE
   encoded text files are misclassified as binary. See comment in `isLikelyBinary`.
2. **O(n) session cache scan**: see `readLastCacheEntry` comment.
3. **Lock timeout drops writes**: see `appendJSONLine` comment. Uses `flock(2)` advisory locks;
   on timeout the write is skipped (not retried).
4. **OpenCode warn mode = no-op**: see Hook protocol (OpenCode) section above.
5. **Static bypass list**: `shouldBypassPath` has a hardcoded list of path segments to skip
   (`.git/`, `node_modules/`, etc.). New patterns must be added manually or via
   `READ_ONCE_EXCLUDE`.
6. **Pi extension uses sha256**: Node.js crypto has no xxhash. sha256 is ~10% slower for
   small files but negligible for our use case (one hash per tool call).
7. **Conservative Bash command parsing**: The Bash parser extracts file paths from 30 reader
   verbs but does not parse command-specific flags. A `head -n 5 file.txt` caches under just
   `file.txt`, blocking all future reads of that file (including full reads). This is
   intentionally conservative — partial reads are never served from cache.
8. **Bash `cd` chaining**: The parser handles `cd dir && cat file` and `cd dir ; cat file`
   patterns by resolving the file path relative to the `cd` target. This matches Codex's
   behavior for navigating to subdirectories before reading files.
9. **Shell wrapper unwrapping**: The parser unwraps `bash -lc 'cmd'` and `zsh -lc 'cmd'`
   wrappers, extracting and parsing the inner command. This matches Codex's pattern of
   wrapping commands in shell invocations.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `READ_ONCE_MODE` | `warn` | `warn` / `deny` / `allow` |
| `READ_ONCE_MODE_UNCHANGED` | inherits `READ_ONCE_MODE` | Mode for unchanged-file re-reads |
| `READ_ONCE_MODE_CHANGED` | inherits `READ_ONCE_MODE` | Mode for changed-file re-reads |
| `READ_ONCE_TTL` | `300` | Base cache TTL in seconds (adaptive scales up to 3x) |
| `READ_ONCE_DIFF` | `0` | `1` = send diff for changed files instead of full re-read |
| `READ_ONCE_DIFF_MAX` | `40` | Max diff lines before falling back to summary |
| `READ_ONCE_HASH` | `1` | `1` = validate unchanged reads by content hash |
| `READ_ONCE_HASH_ALGO` | `xxhash` | `xxhash` or `sha256` (Go); always sha256 (pi) |
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
