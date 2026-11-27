# Changelog

All notable changes to this project are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.7.0] - 2026-11-26

### Added

- Local API server mode (`--serve [ADDR]`, default `127.0.0.1:8080`, and
  `--serve-key KEY` / `DEEPSEEK_SERVE_KEY`) enabling OpenAI/Anthropic SDKs and
  coding agents (OpenCode, Codex CLI, Claude Code, Cline, Roo Code, Aider) to
  connect directly.
- Multi-protocol endpoints: `GET /health`, `GET /v1/models`,
  `POST /v1/chat/completions` (OpenAI Chat Completions), `POST /v1/responses`
  (OpenAI Responses API with `previous_response_id`), and `POST /v1/messages`
  (Anthropic Messages API).
- Virtual model routing for `deepseek-chat`, `deepseek-reasoner`,
  `deepseek-chat-search`, and `deepseek-reasoner-search`, plus `web_search` tool
  and `web_search_options` mapping.
- Dual-format streaming and non-streaming tool-call parser supporting both
  `<tool_call>` JSON blocks and DeepSeek's native DSML tags
  (`<｜DSML｜...>` / `<｜｜DSML｜｜...>`) across split SSE chunks.
- Prefix-matched session cache (`serveSessionCache`) and concurrency limiter so
  multi-turn agent tool loops stay within one DeepSeek chat session.
- Comprehensive unit and mock-upstream tests in `test/server_test.go`.

### Fixed

- `APIClient.Complete` (`api.go`) now detects HTTP `200 OK` JSON error payloads
  (such as `40003 INVALID_TOKEN` or `40303 CHALLENGE_REQUIRED`) instead of
  silently returning an empty completion, and `--serve` maps them to proper HTTP
  error responses (`403` / `502`).

## [0.6.0] - 2026-11-20

### Added

- Document & File Attachments: support for uploading and attaching local documents
  via `/api/v0/file/upload_file` with native `DeepSeekHashV1` proof-of-work negotiation.
- Terminal Drag-and-Drop: dragging and dropping any file from your desktop or file
  manager into the TUI automatically parses the file path, strips quotes/URI schemes,
  unescapes special characters, and sets it up as an attachment.
- CLI File Attachment: added `-f` / `--file PATH` flag in CLI mode, as well as automatic
  path detection for dropped files in the CLI prompt argument.
- TUI Mode Toggles: hotkeys `ctrl+d` (toggle DeepThink reasoning mode on/off) and
  `ctrl+s` (toggle web search retrieval on/off) with live header badges `[think]` / `[search]`.
- CLI Mode Toggles: added `--think`, `--no-think`, `--search`, and `--no-search` flags.
- Mid-Stream Cancellation: press `esc` in the TUI during active streaming to abort in-flight
  generation immediately without losing prior chat turns or exiting the application.
- Conversation History Export: export entire chat sessions with collapsible thinking
  process blocks to clean Markdown via `e` in the recent chats drawer or `-e` / `--export ID` in CLI mode.
- Comprehensive `--help` output with full CLI flags, TUI hotkeys, and attachment instructions.

### Changed

- Enhanced error envelope parsing to properly unwrap server business errors (`biz_code` / `biz_msg`)
  when payloads return null business data.
- Improved empty conversation view with discovery hint for file attachments.

## [0.5.1] - 2026-11-17

### Added

- `-1` / `--first` to print only the first selected code block, useful for
  redirecting one script to a file.
- `-l` / `--lang` to filter blocks by language, including comma-separated
  lists. `shell` matches `sh`, `bash`, `zsh`, and `fish`; common pairs such as
  `js`/`javascript`, `py`/`python`, `ts`/`typescript`, and `golang`/`go` are
  recognized.
- Automatic code-only instruction in `-c` mode asking for one fenced block
  with all commands inside it. `--no-hint` sends the prompt unchanged.
- Tests for language labels, filtering, selection, and prompt construction.

### Changed

- `-c` now exits with status 3 when no fenced block is found or no block
  matches `--lang`, instead of silently returning 0.
- Code-mode answers are more likely to contain a single final script instead
  of several alternatives.

## [0.5.0] - 2026-11-17

### Added

- Non-interactive CLI mode: passing prompt arguments runs a one-shot query and
  streams the visible answer to stdout. Running with no arguments still opens
  the interactive TUI.
- `-c` / `--code` flag that prints only the contents of fenced Markdown code
  blocks, discarding surrounding conversational text.
- Argument parsing that accepts flags before or after the prompt, supports
  `--` as an end-of-flags marker, and joins all non-flag arguments into one
  prompt.
- `-h` / `--help` output documenting CLI usage, exit codes, and token lookup.
- Tests for argument parsing, code-block extraction, and the CLI completion
  payload.

### Changed

- Thinking is enabled for TUI requests but disabled for CLI requests, keeping
  internal monologue out of stdout.
- `--version` and token-only `--token` invocations keep their previous
  behavior; `--token` combined with a prompt enters CLI mode.

## [0.4.0] - 2026-11-17

### Added

- Assistant replies are rendered as terminal Markdown: headings, emphasis,
  inline code, links, ordered and unordered lists, task lists, blockquotes,
  horizontal rules, tables, and fenced code blocks with language labels and
  preserved indentation.
- Per-install device identity, locale, timezone, and user-agent values.

### Security

- Removed the shared client identity used by earlier builds. The repository
  history was rewritten to purge the embedded identifier; older clones must be
  re-cloned.

[0.7.0]: https://github.com/chriz-3656/deepseek-tui/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/chriz-3656/deepseek-tui/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/chriz-3656/deepseek-tui/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/chriz-3656/deepseek-tui/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/chriz-3656/deepseek-tui/releases/tag/v0.4.0
