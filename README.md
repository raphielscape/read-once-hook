# read-once-hook

`read-once` is a pre-tool hook that deduplicates repeated file reads in an agent session.

## Hook Runtime Notes (Codex vs Claude)

If hooks are enabled, Codex may show lifecycle lines such as:

- `running PreToolUse hook`
- `PreToolUse hook (completed)`

This text is emitted by the client runtime UI/event stream, not by this project's hook logic.
The hook itself only returns JSON decisions/messages through stdout.

Claude and Codex can present hook activity differently, so seeing these lines in Codex but 
not in Claude is expected.

## Current Install Paths

- Claude defaults: `~/.claude/read-once/read-once hook`
- Codex defaults: `~/.codex/read-once/read-once hook`
- OpenCode defaults: `~/.config/opencode/read-once/read-once` + `~/.config/opencode/plugins/read-once.js`

## Tracking Notes for Codex Bash

The binary hook tracks reads from common direct and piped patterns, including commands like:

- `cat file`
- `rg pattern file | head -n 20`
- `grep pattern file`
- `awk ... file`
- `nl file`
- `git show REV:path` (mapped to the local `path` when available)

If a command is skipped, enable diagnostics with:

- `READ_ONCE_DEBUG=1`

This writes skip reasons to `$READ_ONCE_HOME/debug.log` (or the default client cache dir).
