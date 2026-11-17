# deepseek-tui — Project Summary

> Status snapshot: **2026-11-17** · Latest release: **v0.4.0** · Language: **Go 1.24.2** · License: **MIT**

## 1. Project at a glance

`deepseek-tui` is a single-binary terminal UI for DeepSeek chat. It talks
directly to the DeepSeek web backend with a browser `userToken` and does not
need a browser, a JavaScript sidecar, or a WASM runtime.

The application provides:

- a Bubble Tea terminal interface with streaming replies;
- a native, pure-Go `DeepSeekHashV1` proof-of-work solver;
- multi-turn conversations using server-side session state;
- a recent-chats drawer with history restoration, pagination, switching, and
  deletion;
- Markdown-rendered assistant replies with headings, lists, tables, and
  fenced code blocks;
- token discovery and an in-TUI token entry screen;
- cross-platform release binaries for Linux, macOS, and Windows.

Repository: `https://github.com/chriz-3656/deepseek-tui`

## 2. What was actually built

### 2.1 Terminal UI

- Bubble Tea model/update/view architecture.
- Alternate-screen layout with:
  - header and connection/status indicator;
  - scrollable chat viewport;
  - single-line message input;
  - spinner while connecting, solving PoW, loading chats, or deleting;
  - adaptive light/dark colors.
- Streaming assistant output:
  - `THINK` fragments rendered dim and italic;
  - `RESPONSE` fragments rendered through the Markdown renderer;
  - a cursor block while text is arriving.
- Status progression for:
  - creating a chat session;
  - requesting the PoW challenge;
  - solving the challenge with progress;
  - waiting for DeepSeek;
  - streaming the answer.
- Responsive layout that recalculates the viewport on terminal resize.

### 2.2 Native proof-of-work

Implemented in `pow.go`:

- `DeepSeekHashV1` is **not** SHA3-256 and **not** standard Keccak-256.
- It uses:
  - Keccak-f[1600] rounds 1 through 23 (23 rounds instead of 24);
  - SHA3-style `0x06` padding with `0x80` at the end;
  - a 136-byte rate;
  - a 32-byte digest read from the first four little-endian lanes.
- Nonce inputs fit in one rate block, so each candidate costs one permutation.
- The solver:
  - runs up to 8 workers, capped by `GOMAXPROCS`;
  - claims nonces in 1024-nonce chunks using an atomic counter;
  - cancels the remaining workers after the first match;
  - reports progress through a callback;
  - honors context cancellation.
- The test suite pins two captured `(salt, expire_at, answer)` vectors from
  the live web client and verifies the exact digests.

### 2.3 DeepSeek API client

Implemented in `api.go`:

- Browser-like request headers matching the web client.
- Standard DeepSeek envelope decoding:
  - flat responses;
  - `{"code":0,"data":{"biz_data":...}}`;
  - nested `chat_session` shapes.
- Tolerant numeric decoding for IDs and timestamps that may be JSON numbers
  or numeric strings.
- Structured `APIError` handling with status, code, and message.
- Friendly user-facing errors for:
  - `401`/`403`: token rejected;
  - `429`: rate limited;
  - other upstream failures: raw API error text.

### 2.4 Streaming SSE parser

The parser supports both observed DeepSeek stream protocols:

1. Simple patch protocol:
   - paths such as `response/content`;
   - operations such as `APPEND`;
   - continuation values with no path.
2. Fragment/snapshot protocol:
   - `response/fragments` snapshots;
   - `response/fragments/-1/content` appends;
   - `THINK`, `THINKING`, or `REASONING` fragments;
   - `RESPONSE` fragments;
   - `ready` events carrying `response_message_id`.

The `ready` message ID becomes the next turn's `parent_message_id`.

### 2.5 Multi-turn conversations

- New sessions are created with `POST /api/v0/chat_session/create`.
- The first turn sends `model_type: "default"`.
- Subsequent turns send the current `parent_message_id`.
- `FirstTurn` is based on `parentID == nil`, not on whether the session ID is
  empty. This keeps reopened sessions with no parent message correct.
- Loaded history restores both the visible messages and the next parent ID.

### 2.6 Recent chats

Added in v0.2.0:

- `ctrl+r` opens the recent-chats drawer.
- Lists server-side sessions newest first.
- Supports cursor pagination with `has_more`.
- Select with `↑`/`↓` or `j`/`k`; open with `enter`.
- `l` loads the next page.
- `r` refreshes the list.
- `n` starts a new chat.
- `d` deletes the selected chat after `y` confirmation.
- `esc` closes the drawer.
- The active session is marked, and pinned sessions get their own marker.
- Relative timestamps are shown when the panel is wide enough.
- The active chat title is displayed in the main header.
- Deleting the active chat resets the conversation; deleting another session
  keeps the current conversation intact.
- Stale asynchronous responses are ignored through sequence counters.

### 2.7 Authentication and token handling

Token lookup order:

1. `--token` command-line flag.
2. `DEEPSEEK_TOKEN` environment variable.
3. `~/.config/deepseek.token`.
4. In-TUI token screen.

Details:

- A leading `Bearer ` prefix is stripped.
- The token screen masks input with dots by default and supports normal
  typing and bracketed paste (`ctrl+v` or right-click).
- `ctrl+r` toggles token visibility while editing.
- The token is saved with mode `0600`.
- `ctrl+t` reopens the token screen during a session.
- If no token exists and stdin is not a terminal, the program exits with a
  clear error instead of trying to render a TUI.

### 2.8 Per-install client identity

Nothing identity-related is baked into the executable. On startup the client
derives:

- **Device id:** UUIDv4 generated on first run, stored at
  `~/.config/deepseek.device` with mode `0600`, and reused on later runs.
  `DEEPSEEK_DEVICE_ID` overrides it.
- **Locale:** first valid value from `LC_ALL`, `LC_MESSAGES`, or `LANG`,
  normalized from `xx-YY.UTF-8` to `xx_YY`; fallback `en_US`.
- **Timezone offset:** the current OS UTC offset in seconds.
- **User agent:** a Firefox user agent matching the build platform.
  `DEEPSEEK_USER_AGENT` overrides it.

Protocol constants remain fixed because they belong to DeepSeek's web client:
`x-client-version: 2.8.0`, `x-client-bundle-id: com.deepseek.chat`, the API
base URL, and the completion target path.

### 2.9 Markdown rendering

Assistant messages are rendered as terminal Markdown by `markdown.go`:

- headings, with an underline for level 1;
- bold, italic, strikethrough, inline code, links, and autolinks;
- ordered, unordered, and task lists with nested indentation;
- blockquotes and horizontal rules;
- GitHub-style pipe tables with alignment and box borders, plus a compact
  fallback on narrow terminals;
- fenced code blocks (backticks or tildes) with a language label, preserved
  indentation, expanded tabs, and truncation for overlong lines.

Styling is applied after word wrapping, so ANSI sequences never corrupt the
layout. Open code fences render as code during streaming. Rendered replies
are cached by content length and width, and terminal control characters are
removed before display. Images, raw HTML, and LaTeX are shown as plain text;
there is no syntax highlighting yet.

## 3. Release history

| Tag | Commit | Date | Highlights |
| --- | --- | --- | --- |
| `v0.1.0` | `d5d721d` | 2026-11-16 | Initial TUI, native PoW, SSE streaming, multi-turn sessions, cross-platform release workflow |
| `v0.1.1` | `50a2026` | 2026-11-16 | Fixed typed and pasted token entry inside the TUI |
| `v0.2.0` | `36b6aba` | 2026-11-16 | Recent chats: list, paginate, switch, restore history, delete |
| `v0.3.0` | `b8ee9e8` | 2026-11-17 | Masked token input with `ctrl+r` reveal/hide |
| `v0.3.1` | `f0a99bf` | 2026-11-17 | Per-install device id, derived locale/timezone/user agent, neutral build metadata |
| `v0.4.0` | `b22a497` | 2026-11-17 | Terminal Markdown rendering: headings, lists, tables, code fences, inline styles |

> **Withdrawn builds:** the `v0.1.0`–`v0.3.0` GitHub releases and tags were
> deleted on 2026-11-17. Those binaries embedded a shared device id and fixed
> locale, timezone, and user-agent values, giving every installation the same
> client identity. `v0.3.1` is the only supported release. The repository
> history was rewritten and force-pushed on 2026-11-17 to purge the old
> identifier from every commit. Build only from `v0.3.1` or later.

Supporting commits:

| Commit | Purpose |
| --- | --- |
| `0ab614a` | Initial DeepSeek TUI with native `DeepSeekHashV1` solver |
| `0252067` | Committed `go.sum` and enforced tidy reproducibility in CI |
| `d5d721d` | Recorded indirect modules from `go mod tidy` |
| `cffd4ba` | Added text-wrapping and TUI transition tests |
| `50a2026` | Auth fix for typed and bracketed-paste tokens |
| `36b6aba` | Recent-chats feature |
| `b8ee9e8` | Masked token input with `ctrl+r` toggle |
| `f0a99bf` | Runtime client identity, atomic private files, neutral build metadata |
| `b22a497` | Dependency-free Markdown renderer and render cache |

## 4. What was fixed

### v0.1.1 — token entry

The original token screen only handled a narrow input path. It was fixed to:

- accept normal typed input;
- accept bracketed paste from the terminal;
- normalize whitespace, quotes, and a `Bearer ` prefix;
- save the token correctly;
- show validation errors without losing the input;
- cover the behavior with tests.

### v0.2.0 — correctness fixes that came with recent chats

- Changed session creation payload from `{}` to `{"agent":"chat"}`, matching
  the real web client.
- Changed `FirstTurn` detection to `parentID == nil`, so a reopened session
  without a parent still sends the correct first-turn model type.
- Added tolerant parsing for numeric IDs and timestamps sent as strings.
- Added dual-shape history support:
  - fragment-based messages;
  - top-level `content` / `thinking_content` messages.
- Added request sequence counters to prevent late list/history/delete responses
  from overwriting newer state.
- Added session de-duplication and cursor-advance checks to prevent repeated
  pages or infinite pagination.
- Added a chat-title display in the header.
- Added mocked API and TUI tests for the new flows.

### Build and repository hygiene

- `go.sum` is committed.
- CI fails if `go mod tidy` changes `go.mod` or `go.sum`.
- CI runs vet, race tests, a static build, and a `--version` smoke test.
- Release builds are reproducible with `-trimpath` and version injection.

## 5. API and protocol reference

Base URL:

```text
https://chat.deepseek.com/api/v0
```

### 5.1 Endpoints used

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/chat_session/create` | Create a chat session |
| `POST` | `/chat/create_pow_challenge` | Request a PoW challenge for completion |
| `POST` | `/chat/completion` | Submit a prompt and stream the answer |
| `GET` | `/chat_session/fetch_page` | List recent sessions |
| `GET` | `/chat/history_messages` | Load a session's full history |
| `POST` | `/chat_session/delete` | Delete one session |

### 5.2 Request headers

Every request sets browser-like headers:

```text
Authorization: Bearer <userToken>
Content-Type: application/json
Accept: */*
User-Agent: <Firefox user agent for the build platform, or DEEPSEEK_USER_AGENT>
x-client-version: 2.8.0
x-client-platform: web
x-client-bundle-id: com.deepseek.chat
x-client-locale: <derived from LC_ALL / LC_MESSAGES / LANG, fallback en_US>
x-client-timezone-offset: <current OS UTC offset in seconds>
x-device-id: <per-install UUIDv4 from ~/.config/deepseek.device>
Origin: https://chat.deepseek.com
Referer: https://chat.deepseek.com/
```

### 5.3 Create session

Request:

```json
{"agent":"chat"}
```

Accepted response shapes include:

```json
{"id":"session-id"}
```

```json
{"code":0,"data":{"biz_data":{"chat_session":{"id":"session-id"}}}}
```

```json
{"code":0,"data":{"biz_data":{"id":"session-id"}}}
```

### 5.4 Proof-of-work challenge

Request:

```json
{"target_path":"/api/v0/chat/completion"}
```

The challenge may be returned as an object or as a hex digest. Fields used:

```text
algorithm
challenge
salt
expire_at
difficulty
signature
target_path
```

The client defaults the algorithm to `DeepSeekHashV1` and rejects unsupported
algorithms or incomplete challenges.

### 5.5 Completion

Request body:

```json
{
  "chat_session_id": "<session-id>",
  "parent_message_id": 42,
  "model_type": "default",
  "prompt": "user message",
  "ref_file_ids": [],
  "thinking_enabled": true,
  "search_enabled": true,
  "action": null,
  "preempt": false
}
```

`model_type` is sent only on the first turn (`parent_message_id == null`).
The PoW answer is sent in the `x-ds-pow-response` header as base64-encoded JSON:

```json
{
  "algorithm": "DeepSeekHashV1",
  "challenge": "<hex digest>",
  "salt": "<salt>",
  "answer": 58595,
  "signature": "<signature>",
  "target_path": "/api/v0/chat/completion"
}
```

### 5.6 Streaming events

Handled event names and shapes:

- `ready`: contains `response_message_id`.
- `close`: ends the stream.
- `error`: contains `code` and `msg`.
- `message`, `update_session`, `title`, and empty event names: patch data.

Patch examples:

```json
{"p":"response/content","o":"APPEND","v":"Hello"}
```

```json
{"p":"response/fragments","o":"APPEND","v":[{"id":2,"type":"RESPONSE","content":"Hello"}]}
```

```json
{"p":"response/fragments/-1/content","o":"APPEND","v":" world"}
```

Full snapshots may also contain:

```json
{"v":{"response":{"message_id":42,"fragments":[...]}}}
```

### 5.7 Recent sessions

Request:

```http
GET /api/v0/chat_session/fetch_page?count=50
```

For older pages, add:

```text
lte_cursor.pinned=<bool>
lte_cursor.updated_at=<epoch seconds>
```

Response:

```json
{
  "code": 0,
  "data": {
    "biz_code": 0,
    "biz_data": {
      "chat_sessions": [
        {
          "id": "session-id",
          "title": "Example chat",
          "pinned": false,
          "updated_at": 1782741816.158
        }
      ],
      "has_more": true
    }
  }
}
```

The cursor is built from the last session in the page. If the next cursor does
not advance beyond the requested cursor, pagination stops.

### 5.8 History

Request:

```http
GET /api/v0/chat/history_messages?chat_session_id=<session-id>
```

Response:

```json
{
  "code": 0,
  "data": {
    "biz_data": {
      "chat_session": {
        "id": "session-id",
        "title": "Example chat",
        "current_message_id": 42
      },
      "chat_messages": [
        {
          "message_id": 40,
          "parent_id": null,
          "role": "USER",
          "fragments": [
            {"type": "REQUEST", "content": "question"}
          ]
        },
        {
          "message_id": 41,
          "parent_id": 40,
          "role": "ASSISTANT",
          "fragments": [
            {"type": "THINK", "content": "reasoning"},
            {"type": "RESPONSE", "content": "answer"}
          ]
        },
        {
          "message_id": 42,
          "parent_id": 41,
          "role": "ASSISTANT",
          "content": "alternative flat answer",
          "thinking_content": "alternative flat reasoning"
        }
      ]
    }
  }
}
```

The history endpoint returns the complete conversation in one response; it has
no pagination. Both fragment-based and flat-content shapes are supported.

`current_message_id` becomes the next turn's `parent_message_id`. If it is
missing or not present in the returned messages, the last valid message ID is
used. If no IDs exist, the next turn starts with a null parent.

### 5.9 Delete session

Request:

```json
{"chat_session_id":"<session-id>"}
```

The field is singular and holds one string, not an array.

## 6. Architecture and files

| File | Responsibility |
| --- | --- |
| `main.go` | Bubble Tea model, update loop, views, authentication, recent-chats overlay, text layout, styles, entry point |
| `markdown.go` | Dependency-free Markdown block parser, inline styles, tables, code fences, styled wrapping, render cache |
| `api.go` | HTTP client, headers, envelopes, session/history/delete calls, PoW submission, SSE parser |
| `pow.go` | Keccak-f[1600] 23-round permutation, `DeepSeekHashV1`, parallel solver |
| `main_test.go` | Text wrapping, resize/view, auth input, Markdown view integration, recent-chat switching, delete-current behavior |
| `markdown_test.go` | Headings, inline styles, lists, tasks, quotes, tables, code fences, width limits, sanitization, cache invalidation |
| `api_test.go` | Envelope shapes, PoW header, SSE protocols, pagination, history shapes, delete |
| `pow_test.go` | Captured hash vectors, captured challenge solve, invalid challenge rejection |
| `README.md` | User-facing usage, key bindings, API flow, PoW notes |
| `Makefile` | Build, test, vet, tidy, run, cross-compile, clean |
| `.github/workflows/ci.yml` | Tidy verification, vet, race tests, build, smoke test, release publishing |

### 6.1 TUI state

The main model tracks:

- token, API client, config path, event channel;
- viewport, message input, auth input, spinner;
- auth mode and auth error;
- chat messages, active session ID, chat title, parent message ID;
- recent-chats state:
  - open flag;
  - session list and selected index;
  - pagination cursor and `has_more`;
  - loading/error/notice state;
  - pending history session;
  - delete confirmation/deleting state;
  - sequence counters for list, history, and delete requests;
- streaming state, status text/level, PoW percentage;
- terminal width/height and readiness.

### 6.2 Async and stale-response handling

Network work runs in Bubble Tea commands, and results arrive as messages:

- `sessionsMsg`
- `historyMsg`
- `deleteSessionMsg`
- `streamEvent`

Each recent-chat operation increments a sequence counter. A returned message is
only applied if its sequence matches the current counter and the drawer is
still open. Closing the drawer invalidates all in-flight replies. This avoids
late refreshes or deletions corrupting the current screen.

### 6.3 Recent-chat flow

1. `ctrl+r` opens the drawer and starts a first-page request.
2. While loading, the previous list remains visible.
3. The active session is selected if it appears in the list.
4. `enter` on a different session starts a history request.
5. On success, history is converted to chat messages, the session ID and title
   are set, the parent ID is restored, the drawer closes, and the input is
   focused.
6. `enter` on the already-active session simply closes the drawer.
7. `d` opens a confirmation; `y` starts the delete request; `n`/`esc` cancels.
8. Deleting the active session clears the conversation; otherwise only the
   list changes.

### 6.4 History restoration

For each message:

- `USER`:
  - prefer `fragments[].type == REQUEST`;
  - fall back to top-level `content`.
- `ASSISTANT`:
  - visible text prefers `RESPONSE` fragments, then `content`;
  - reasoning prefers `THINK` fragments, then `thinking_content`.
- Messages with no usable text are skipped.
- `current_message_id` is validated against the returned message IDs before it
  is used as the next parent.

## 7. Build, CI, and releases

### Build locally

```bash
go build -trimpath -buildvcs=false -ldflags "-s -w" -o deepseek-tui .
```

`-buildvcs=false` keeps the repository path, commit hash, and build time out
of the binary's Go build info. The module path is the neutral `deepseek-tui`.

Cross-compile:

```bash
make cross
```

All targets use `CGO_ENABLED=0`.

### CI pipeline

On every push and pull request:

1. `actions/checkout`
2. `actions/setup-go` using `go.mod`
3. `go mod tidy`
4. Fail if `go.mod` or `go.sum` changed
5. `go vet ./...`
6. `go test -race -count=1 ./...`
7. Static Linux build with version injection
8. `--version` smoke test
9. Upload the Linux binary and `go.sum` as artifacts

On a `v*` tag, the release job also:

- builds Linux amd64/arm64, macOS amd64/arm64, and Windows amd64;
- writes `checksums.txt` with SHA-256 sums;
- creates or updates the GitHub release.

### Released assets for v0.3.1

Earlier releases and tags were deleted on 2026-11-17; v0.3.1 is the only
published build.

- `checksums.txt`
- `deepseek-tui-linux-amd64` (~7.2 MB)
- `deepseek-tui-linux-arm64` (~6.8 MB)
- `deepseek-tui-darwin-amd64` (~7.4 MB)
- `deepseek-tui-darwin-arm64` (~7.0 MB)
- `deepseek-tui-windows-amd64.exe` (~7.4 MB)

## 8. Verification performed

- v0.3.1: push CI and tag release both passed; all six assets matched their
  published SHA-256 digests; the Linux binary reported `deepseek-tui v0.3.1`.
- A `strings` audit of the v0.3.1 Linux binary found no shared device UUID, no
  repository handle, no `vcs.revision`/`vcs.time`, and no fixed timezone
  offline. Build info reports `path deepseek-tui`.
- CI passed on the recent-chats commit:
  - tidy check;
  - vet;
  - race tests;
  - build;
  - version smoke test.
- Tag release pipeline passed; all platform assets and checksums were
  published.
- The v0.2.0 Linux release binary reported:

  ```text
  deepseek-tui v0.2.0
  ```

- The TUI was run in a pseudo-terminal with a fake token:
  - the main chat screen rendered;
  - `ctrl+r` opened the recent-chats drawer;
  - the drawer showed its loading state and key help;
  - the real server's `INVALID_TOKEN` error was displayed cleanly.
- No live DeepSeek user token was available in the development environment,
  so live session/history/completion calls were not exercised end-to-end.
  Mocked tests and captured protocol shapes are the current verification
  substitute.

## 9. Known limitations and risks

### Protocol and account

- The project uses an **unofficial web API**. Endpoints, headers, client
  version, response envelopes, and payload shapes may change without notice.
- There is no OAuth login flow; users must extract a `userToken` from the web
  client manually.
- Tokens may expire or be rejected; the app reports this but cannot refresh it.
- No live integration test runs in CI because it would require a real account
  token.

### Session features

- No persistent local history cache. The server remains authoritative, so
  opening a chat requires a network request.
- No rename, pin/unpin, delete-all, search, or export in the UI.
- No offline browsing of previously loaded chats.
- Recent-chat list pagination is implemented, but only the first page is
  fetched automatically.

### Chat features

- No retry, regenerate, stop/cancel stream, or edit-message actions.
- Thinking and web search are always enabled; there is no UI toggle.
- No model selection.
- No file attachments or `ref_file_ids` workflow.
- No message feedback controls.
- No local conversation search.

### Rendering

- Assistant replies use the built-in Markdown renderer; user and system text
  stay plain.
- No syntax highlighting inside fenced code blocks.
- Images and raw HTML render as text.
- Long code lines are truncated with an ellipsis; there is no horizontal
  scroll or copy-friendly code pane.
- Wide tables fall back to compact rows on narrow terminals.
- Nested emphasis and some link edge cases are simplified.
- No LaTeX rendering.

### Platform and operations

- Cross-platform binaries are built, but runtime testing has focused on Linux.
- Device id, locale, timezone, and user agent are derived per install; the
  device id file is the only persistent identity state.
- No configurable theme or config file beyond the token and device id files.
- Rate limiting is surfaced as an error; there is no automatic backoff or retry.
- Preferred delivery of tokens is environment/file/flag; the in-TUI screen is
  convenient but tokens remain sensitive.

## 10. Future work

### 10.1 Implemented: Markdown rendering

Shipped in v0.4.0 as a dependency-free renderer in `markdown.go`:

- block parsing for headings, horizontal rules, fenced code (backticks and
  tildes), blockquotes, ordered/unordered/task lists, and GFM tables;
- inline parsing for bold, italic, strikethrough, inline code, links,
  autolinks, and backslash escapes;
- styled spans wrapped to the viewport width before ANSI styling is applied;
- code blocks with language labels, preserved indentation, expanded tabs, and
  truncation for overlong lines;
- open fences treated as code while a reply is streaming;
- rendered messages cached by content length and width;
- control characters stripped from all rendered text.

Dependency decision: a custom scanner plus Lip Gloss was chosen because it
keeps the binary static and avoids new modules. Chroma remains the best option
for syntax highlighting later, subject to its Go version floor. Glamour was
rejected: v1 pulls in Chroma/Goldmark and a newer Lip Gloss, while v2 requires
Go 1.25 and Lip Gloss v2.

Remaining work: syntax coloring, image/link previews, LaTeX, and richer
nested emphasis.

### 10.2 Near-term improvements

- Syntax highlighting for fenced code blocks.
- Horizontal scrolling or a copy-friendly code view.
- Session management: rename, pin, delete all, search, export.
- Message actions: retry, regenerate, stop/cancel, edit last prompt.
- Toggles for thinking/search/model selection.
- Config file for theme and client defaults beyond the current environment
  overrides.
- File attachments and the `ref_file_ids` upload flow.
- Better rate-limit handling with retry/backoff.
- Live integration tests using a CI secret token.
- Golden tests for terminal rendering and ANSI output.

### 10.3 Longer-term ideas

- Local encrypted history cache for offline browsing.
- Session search and full-text archive/export.
- OAuth or safer token-refresh flow if DeepSeek exposes one.
- Package-manager distribution (Homebrew, Scoop, AUR, etc.).
- Additional themes and accessibility options.
- Optional plugin/MCP-style integrations if the architecture justifies them.

## 11. Key technical decisions

1. **Direct web backend, no browser automation.** Keeps the app a single static
   binary and avoids browser dependencies.
2. **Native PoW.** The 23-round hash variant is implemented in pure Go; no
   JavaScript or WASM sidecar.
3. **Server-authoritative sessions.** Recent chats and history are fetched from
   DeepSeek rather than stored locally.
4. **Exact client-shaped payloads.** Create-session uses `{"agent":"chat"}`;
   completion includes the current parent ID and first-turn model type.
5. **Dual history decoding.** Both fragment and flat-content response shapes
   are supported because the service has served both.
6. **Sequence counters for async UI work.** Prevents stale list/history/delete
   replies from corrupting newer state.
7. **No full Markdown renderer yet.** The next rendering step is deliberately
   scoped to code snippets to avoid changing prose behavior and to keep the
   binary lightweight.
8. **CI-driven build verification.** Because the local environment has no Go
   toolchain with enough resources, compilation, vetting, race tests, and
   release builds run through GitHub Actions.

## 12. Quick start

```bash
go build -trimpath -ldflags "-s -w" -o deepseek-tui .
export DEEPSEEK_TOKEN="<web userToken>"
./deepseek-tui
```

Token alternatives:

```bash
./deepseek-tui --token "<web userToken>"
```

or write the token to `~/.config/deepseek.token` with mode `0600`.

### Key bindings

| Key | Chat view | Recent chats |
| --- | --- | --- |
| `enter` | Send message / save token | Open selected chat |
| `ctrl+r` | Open recent chats | — |
| `ctrl+n` | New chat | New chat |
| `ctrl+t` | Change token | Change token |
| `up` / `down` | Scroll history | Select chat |
| `pgup` / `pgdown` | Scroll history | Move by a page |
| `home` / `end` | Top / bottom | First / last chat |
| `r` | — | Refresh list |
| `l` | — | Load more sessions |
| `d` | — | Delete selected chat |
| `y` / `n` | — | Confirm / cancel delete |
| `esc` | — | Close drawer |
| `ctrl+c` | Quit | Quit |

## 13. References

- Repository: `https://github.com/chriz-3656/deepseek-tui`
- Releases: `https://github.com/chriz-3656/deepseek-tui/releases`
- DeepSeek web chat: `https://chat.deepseek.com`
- Bubble Tea: `https://github.com/charmbracelet/bubbletea`
- Bubbles: `https://github.com/charmbracelet/bubbles`
- Lip Gloss: `https://github.com/charmbracelet/lipgloss`
- Chroma: `https://github.com/alecthomas/chroma`
- Glamour: `https://github.com/charmbracelet/glamour`
- `x/ansi`: `https://github.com/charmbracelet/x/tree/main/ansi`

---

This document records the project as of v0.2.0. The code-rendering section is
research and a proposed plan; it has not been implemented yet.
