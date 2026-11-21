# deepseek-tui — Project Summary

> Status snapshot: **2026-11-21** · Latest release: **v0.6.0** · Language: **Go 1.24+** · License: **MIT**

## 1. Project at a glance

`deepseek-tui` is a single-binary terminal client and CLI tool for DeepSeek chat. It communicates directly with DeepSeek's web backend using a browser `userToken`, requiring no headless browser, JavaScript runtime, or WASM sidecar.

The application provides:

- **Interactive Terminal UI**: Built with Bubble Tea and Lip Gloss, featuring live streaming replies, responsive layouts, and adaptive color themes;
- **Native Proof-of-Work Engine**: Pure-Go implementation of `DeepSeekHashV1` (a custom 23-round Keccak-f[1600] permutation) solving challenges for both chat completions and file uploads in milliseconds;
- **Document & File Attachments**: Local file upload pipeline (`/api/v0/file/upload_file`) with native PoW challenge solving, terminal drag-and-drop path auto-detection, `:file <path>` TUI command, and `-f, --file` CLI flag;
- **Runtime Mode Toggles**: Live switches for DeepThink reasoning (`ctrl+d`, `--think` / `--no-think`) and Web Search retrieval (`ctrl+s`, `--search` / `--no-search`) with status badges in the header;
- **Mid-Stream Cancellation**: Immediate stream abortion via `esc` in the TUI without dropping prior chat turns;
- **Markdown Session Export**: Exporting full multi-turn conversation histories with collapsible `<details>` thinking blocks to standard GitHub Flavored Markdown (`e` in recent chats drawer, `-e, --export` in CLI);
- **Headless / Stealth CLI Mode**: Non-interactive prompt execution (`deepseek-tui "prompt"`) with code-only extraction filters (`-c, --code`, `-1, --first`, `-l, --lang LANG`);
- **Multi-Turn Session State**: Server-authoritative chat sessions, title tracking, parent message chaining, and a recent-chats drawer with pagination and deletion;
- **Dependency-Free Markdown Renderer**: Built-in renderer supporting headings, styled spans, task lists, GFM tables, and fenced code blocks;
- **Dedicated Test Suite**: Cleanly segregated `test/` package with extensive unit tests and end-to-end integration tests using isolated mock HTTP servers via `DEEPSEEK_BASE_URL`;
- **Cross-Platform Releases**: Pre-built static binaries for Linux, macOS, and Windows across `amd64` and `arm64`.

Repository: `https://github.com/chriz-3656/deepseek-tui`

---

## 2. What was actually built

### 2.1 Terminal UI

- **Bubble Tea Architecture**: Clean Elm-architecture model/update/view loop.
- **Alternate-Screen Viewport**:
  - Header displays active title, status spinner, and mode badges (`[think]` and `[search]`).
  - Scrollable chat viewport with smooth scrolling and auto-scroll to bottom.
  - Single-line message input with command detection.
  - Status progression tracking every stage: session creation, PoW challenge acquisition, parallel solving progress, server waiting, and token streaming.
- **Dynamic Hotkeys**:
  - `ctrl+d`: toggles DeepThink reasoning mode on / off in real time.
  - `ctrl+s`: toggles Web Search retrieval on / off in real time.
  - `esc`: cancels active response generation immediately while streaming.
  - `ctrl+r`: toggles recent chats drawer.
  - `ctrl+n`: resets active conversation and starts a fresh session.
  - `ctrl+t`: opens token entry panel with `ctrl+r` token reveal/hide toggle.
- **Drag-and-Drop Auto-Formatting**:
  - Dragging and dropping a file from desktop or terminal file managers formats the input line into `:file <path> <optional prompt>`.
  - Automatically handles double quotes, single quotes, backslash-escapes, raw spaces, and `file://` URI schemes.
- **Recent Chats Drawer**:
  - Slide-over overlay with session counts, pagination (`l`), deletion (`d` then `y`), and switching (`enter`).
  - Markdown history export with `e` hotkey.

### 2.2 Native Proof-of-Work (`DeepSeekHashV1`)

Implemented in `pow.go`:

- `DeepSeekHashV1` is **not** SHA3-256 and **not** standard Keccak-256.
- Algorithm properties:
  - Keccak-f[1600] rounds 1 through 23 (23 rounds instead of standard 24);
  - SHA3-style `0x06` padding byte with `0x80` at the rate boundary;
  - 136-byte rate block;
  - 32-byte digest taken from the first four 64-bit little-endian state lanes.
- Multi-core parallel solver:
  - Runs workers up to `min(GOMAXPROCS, 8)`.
  - Divides difficulty into 1,024-nonce chunks using atomic counters.
  - Supports cancellation via `context.Context` and progress callbacks (`OnPowProgress`).
- General target support:
  - Negotiates challenges for chat completions (`/api/v0/chat/completion`).
  - Negotiates challenges for file uploads (`/api/v0/file/upload_file`).
  - Encodes the result into the base64-encoded `x-ds-pow-response` header.

### 2.3 Document & File Attachment Pipeline

Implemented across `api.go` and `main.go`:

- **Upload Endpoint**: POST to `/api/v0/file/upload_file` using `multipart/form-data`.
- **Validation**: Enforces a 50MB file size limit and prevents directory uploads.
- **PoW Negotiation**: Requests a proof-of-work challenge specifically keyed for `uploadTargetURL` (`/api/v0/file/upload_file`), solves it, and attaches the resulting `x-ds-pow-response` header.
- **Completion Attachment**: Captures the returned `file_id` and attaches it into the completion request envelope under `ref_file_ids: [file_id]`.
- **Automatic Fallback**: If an attachment is submitted without an explicit user prompt, the prompt defaults automatically to `"Analyze the attached file."`.

### 2.4 Headless / Stealth CLI Mode & Code Extraction

Implemented across `main.go` and `test/stealth_test.go`:

- **One-Shot Execution**: Running `deepseek-tui "prompt"` runs non-interactively, streams the assistant's answer directly to stdout, and exits.
- **Code-Only Output**:
  - `-c, --code`: parses incoming Markdown blocks, strips conversational prose, and prints only code bodies.
  - `-1, --first`: prints only the first matching code block.
  - `-l, --lang LANG`: filters code blocks by programming language (supports comma-separated lists and language aliases like `sh`/`bash`/`zsh`, `py`/`python`, `go`/`golang`, `ts`/`typescript`).
- **Prompt Hints**: In `-c` mode, automatically injects a prompt hint requesting a single fenced code block containing all code (`--no-hint` disables this).
- **Exit Code Standards**:
  - `0`: Successful completion.
  - `1`: General runtime error or missing authentication token.
  - `2`: Invalid CLI arguments or non-existent file path.
  - `3`: No code block matched the requested `--lang` or `--code` filter.

### 2.5 Markdown Conversation Export

Implemented in `api.go` (`ChatHistory.ToMarkdown`) and CLI/TUI:

- **Format**:
  - Generates standard GitHub Flavored Markdown.
  - Document heading: `# <Session Title>`.
  - User turns: `### User\n\n<Prompt>`.
  - Assistant turns: `### DeepSeek`.
  - Thinking turns: `<details>\n<summary>Thinking Process</summary>\n\n<Thinking>\n\n</details>`.
  - Turn dividers: `---` separating subsequent turns.
- **Access**:
  - TUI: Press `e` on any selected chat in the recent chats drawer.
  - CLI: Run `deepseek-tui -e <session-id>` or `deepseek-tui --export <session-id>`.

### 2.6 Terminal Markdown Rendering

Implemented in `markdown.go`:

- Dependency-free block scanner parsing headings, task lists, tables, blockquotes, horizontal rules, and fenced code.
- Preserves indentation and handles tab expansion inside code blocks.
- Viewport width wrapping with styled span calculations and ANSI escape preservation.
- LRU render caching keyed by text content and terminal width.

---

## 3. Release history

| Tag | Date | Highlights |
| --- | --- | --- |
| `v0.6.0` | 2026-11-20 | File attachments (`/file/upload_file` PoW), terminal drag-and-drop auto-detection, reasoning & search toggles (`ctrl+d`, `ctrl+s`, `--think`, `--no-search`), stream cancellation (`esc`), markdown session export (`e`, `--export`), and dedicated `test/` suite |
| `v0.5.1` | 2026-11-17 | Code extraction filters (`-1, --first`, `-l, --lang`), language aliases, prompt hint injection, and exit code 3 |
| `v0.5.0` | 2026-11-17 | Non-interactive headless CLI mode (`deepseek-tui "prompt"`), code-only output (`-c, --code`), flexible flag placement |
| `v0.4.0` | 2026-11-17 | Dependency-free terminal Markdown renderer (headings, tables, lists, code fences, styled wrapping) |
| `v0.3.1` | 2026-11-17 | Per-install runtime client identity (device ID, locale, timezone, user-agent) and atomic private file storage |
| `v0.3.0` | 2026-11-17 | Masked token input with `ctrl+r` reveal/hide |
| `v0.2.0` | 2026-11-16 | Recent chats drawer: list, paginate, switch, restore history, and delete |
| `v0.1.1` | 2026-11-16 | Typed and bracketed-paste token handling |
| `v0.1.0` | 2026-11-16 | Initial Bubble Tea TUI, pure-Go `DeepSeekHashV1` PoW solver, SSE streaming, cross-platform release workflow |

---

## 4. Key fixes and evolutions

### v0.6.0
- **File Upload Error Decoding**: Restructured `decodeEnvelopeData` so server business errors (`biz_code != 0`) are caught even when `biz_data` is null (e.g. unsupported file types).
- **Drag-and-Drop Path Sanitization**: Resolved raw terminal drag-and-drop inputs containing spaces, double quotes, single quotes, backslash-escapes, and `file://` URI encoding.
- **Test File Organization**: Moved all test files from root `package main` into the dedicated `test/` folder while maintaining full coverage and POSIX compliance.
- **Dynamic Test Base URL**: Added `DEEPSEEK_BASE_URL` override support to `NewAPIClient` to allow isolated mock-server end-to-end integration testing.

### v0.5.1 & v0.5.0
- **Exit Code 3 for Unmatched Code Blocks**: `-c` exits with code 3 when no code blocks match `--lang`, allowing shell scripts to detect empty matches.
- **Hint Injection**: Injected code-only system prompt hint when `-c` is passed to discourage conversational preambles.
- **Thinking Default in CLI**: Disabled thinking by default in CLI stealth queries to prevent monologue pollution on stdout.

### v0.3.1 & v0.4.0
- **Per-Install Client Identity**: Eliminated hardcoded client identifiers. Device ID, locale, timezone offset, and user-agent are derived dynamically per installation.
- **Neutral Build Metadata**: Enforced `-buildvcs=false` and `-trimpath` to prevent local build paths or commit hashes leaking into binaries.

---

## 5. API and protocol reference

### Endpoints used

| Endpoint | Method | Purpose |
| --- | --- | --- |
| `/api/v0/chat_session/create` | `POST` | Opens a new chat session (`{"agent":"chat"}`) |
| `/api/v0/chat/create_pow_challenge` | `POST` | Requests a PoW challenge (`target_path`) |
| `/api/v0/file/upload_file` | `POST` | Uploads a local document (`multipart/form-data` with PoW) |
| `/api/v0/chat/completion` | `POST` | Submits prompt, parent ID, toggles, `ref_file_ids`, and streams SSE |
| `/api/v0/chat/history_messages` | `GET` | Retrieves full message history for session export or restoration |
| `/api/v0/chat_session/fetch_page` | `GET` | Paginates through recent chat sessions |
| `/api/v0/chat_session/delete` | `POST` | Deletes a chat session |

### Headers and Identity

Requests carry the following headers:
- `Authorization: Bearer <token>`
- `x-client-platform: web`
- `x-client-version: 1.0.0`
- `x-client-bundle-id: com.deepseek.chat`
- `x-client-locale: <derived locale, e.g. en_US>`
- `x-client-timezone-offset: <offset minutes, e.g. -300>`
- `x-device-id: <persistent UUID from ~/.config/deepseek.device_id>`
- `User-Agent: <derived system user agent>`
- `Origin: https://chat.deepseek.com`
- `Referer: https://chat.deepseek.com/`
- `x-ds-pow-response: <base64 payload>` (on completion and file upload)

---

## 6. Architecture and files

```
deepseek-tui/
├── main.go                     # Entry point, TUI model/update/view, CLI flag parsing, stealth mode
├── api.go                      # HTTP client, PoW submission, SSE parser, file uploads, history export
├── clientenv.go                # Runtime device ID, locale, timezone, user-agent derivation
├── markdown.go                 # Terminal Markdown renderer, inline styling, tables, code fences
├── pow.go                      # Keccak-f[1600] permutation, DeepSeekHashV1 solver
├── Makefile                    # Build, test, cross-compile, and clean targets
├── go.mod / go.sum             # Go module definition (Go 1.24+)
├── README.md                   # User documentation, installation, and usage guide
├── CHANGELOG.md                # Detailed release notes per version
└── test/                       # Dedicated test suite directory
    ├── api_test.go             # API client, envelope parsing, PoW headers, SSE protocols
    ├── clientenv_test.go       # Device ID persistence, permissions, locale, UA derivation
    ├── main_test.go            # TUI state transitions, drag-and-drop parsing, hotkey toggles
    ├── markdown_test.go        # Markdown block parsing, styling, tables, render cache
    ├── pow_test.go             # Captured DeepSeekHashV1 vectors and challenge solving
    ├── stealth_test.go         # CLI flag parsing, code block extraction, language filtering
    └── e2e_test.go             # End-to-end binary execution with mock HTTP server
```

---

## 7. Build, CI, and releases

### Local compilation

```bash
# Build local binary
make build

# Run all unit and integration tests with race detector
make test

# Verify cross-compilation for all release targets
make cross
```

### GitHub Actions CI/CD

On every push to `main` and version tag (`v*`):
1. **Verification**: `go mod tidy` verification, `go vet ./...`, and `go test -race -count=1 ./...`.
2. **Static Build**: `CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=..." .`.
3. **Smoke Check**: Verifies `./dist/deepseek-tui-linux-amd64 --version`.
4. **Automated Publishing**: On `v*` tags, compiles 5 cross-platform targets (Linux amd64/arm64, macOS amd64/arm64, Windows amd64), computes SHA-256 checksums, and publishes assets to GitHub Releases.

---

## 8. Verification performed

- **CI Automation**:
  - Workflow run `44751313565`: `v0.6.0` release assets published.
  - Workflow run `44771940889`: `main` CI passed completely across `tidy`, `vet`, `test`, `build`, and `smoke`.
- **Test Suite Coverage**:
  - `TestDeepSeekHashV1Vectors`: Validates Keccak-23 round hash vectors against live DeepSeek captures.
  - `TestSolveCapturedChallenge`: Validates nonces against captured server challenges.
  - `TestUploadFile`: Exercises multipart form assembly, PoW negotiation, and envelope parsing.
  - `TestTUIDragAndDropAutoFormat`: Tests path unquoting, escaping, and format parsing.
  - `TestToggleThinkingAndSearchInTUI`: Validates runtime hotkey state toggling.
  - `TestStreamCancellationOnEsc`: Validates stream cancellation without state loss.
  - `TestChatHistoryToMarkdown`: Validates Markdown generation with collapsible thinking process blocks.
  - `TestCLIStealthCodeExtraction`: End-to-end integration test verifying full mock SSE stream parsing and code filtering.

---

## 9. Quick start & Key bindings

### Installation & Launch

```bash
# Using Go
go install github.com/chriz-3656/deepseek-tui@latest

# Set your token
export DEEPSEEK_TOKEN="<web userToken>"

# Launch interactive TUI
deepseek-tui
```

### CLI Command Options

| Command / Flag | Purpose |
| --- | --- |
| `deepseek-tui` | Launch the interactive Terminal User Interface |
| `deepseek-tui "prompt"` | One-shot query mode; streams response to stdout and exits |
| `-c, --code` | Output only fenced code block bodies, discarding surrounding text |
| `-1, --first` | Output only the first code block (implies `--code`) |
| `-l, --lang LANG` | Filter code blocks by language (e.g. `-l python`, `-l bash,sh`) |
| `-f, --file PATH` | Attach a local file or document to the query |
| `-e, --export ID` | Export full conversation session as Markdown to stdout |
| `--think` / `--no-think` | Enable or disable DeepThink reasoning mode |
| `--search` / `--no-search` | Enable or disable Web Search retrieval |
| `--token TOKEN` | DeepSeek userToken (overrides environment and config file) |
| `-h, --help` | Display help and usage instructions |
| `--version` | Print version information |

### Interactive TUI Hotkeys

| Key | Chat View | Recent Chats Drawer |
| --- | --- | --- |
| `enter` | Send message / attachment | Open selected chat |
| `ctrl+d` | Toggle DeepThink reasoning mode on / off | — |
| `ctrl+s` | Toggle Web Search retrieval on / off | — |
| `esc` | Cancel active response stream | Close drawer |
| `ctrl+r` | Open recent chats drawer | — |
| `ctrl+n` | Start a new conversation | Start a new conversation |
| `ctrl+t` | Open token manager | Open token manager |
| `up` / `down` | Scroll chat history | Navigate session list |
| `pgup` / `pgdn`| Scroll chat history | Page through session list |
| `e` | — | Export selected chat to Markdown |
| `d` then `y` | — | Delete selected chat |
| `r` | — | Refresh session list |
| `l` | — | Load more session pages |
| `ctrl+c` | Quit application | Quit application |
