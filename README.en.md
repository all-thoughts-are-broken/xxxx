# xxxx

**Comic downloader · image descrambler · PDF builder** — a passive process that speaks JSON Lines.

[![CI](https://github.com/all-thoughts-are-broken/xxxx/actions/workflows/ci.yml/badge.svg)](https://github.com/all-thoughts-are-broken/xxxx/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/all-thoughts-are-broken/xxxx)](https://github.com/all-thoughts-are-broken/xxxx/releases/latest)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**English | [简体中文](README.md)**

> This is a condensed English overview. The [Chinese README](README.md) is the
> authoritative document — it covers the protocol in full, every command, all
> configuration keys, and a long section of implementation notes explaining the
> non-obvious behaviour that must not be "cleaned up".

---

## What it is

A single Go binary that wraps the whole pipeline — *fetch album → descramble the
server-side shuffled images → build a bookmarked PDF* — and exposes it over a
**JSON Lines** protocol instead of a library API.

It **does not parse command-line arguments**. It reads one JSON request per line
from `stdin`, writes JSON responses and progress events to `stdout`, and logs to
`stderr`. Any host that can `spawn` a child process (Node, Python, Go, …) can
drive it, with no other external tools required.

```
host (Node / Python / …)  --stdin: JSON requests-->  xxxx
                          <--stdout: responses + progress events--
                          <--stderr: logs--
```

Why a separate process rather than a library? Encryption, image descrambling and
PDF layout are messy; linking them into a host would tie the host's runtime
(Node version, Python interpreter, packaging) to them. A static binary keeps the
boundary clean and language-agnostic — upgrading means swapping one file.

## Features

- **One command for the whole pipeline** — `download_album` runs probe → concurrent
  download → descramble → PDF → optional merge/encrypt, reporting progress events.
- **Self-probing API endpoints** — the official endpoint list is a rotating,
  encrypted file mirrored on several hosts; the tool fetches it, races the
  candidates concurrently and picks the lowest-latency one, falling back to a
  built-in list.
- **No silent failures** — a missing image becomes a placeholder instead of being
  skipped (skipping would shift every following page number), a failing chapter
  does not abort the run, and per-image failure reasons are reported.
- **Bookmarked PDFs** — chapters as top-level bookmarks, pages nested underneath,
  preserved through merges.
- **Resumable** — `skip_existing` makes re-runs cheap.
- **Image rendering** — album detail cards and comment threads as PNGs.
- **Minimal protocol** — one JSON object per line; machine-readable error codes.

## Install

### Prebuilt binaries

Grab the archive for your platform from
[Releases](https://github.com/all-thoughts-are-broken/xxxx/releases/latest).
No Go toolchain or external tools needed.

| Platform | Architecture |
|---|---|
| Linux | `amd64`, `arm64` |
| macOS | `amd64` (Intel), `arm64` (Apple Silicon) |
| Windows | `amd64` |

Each release ships a `checksums.txt` with SHA-256 sums.

### From source

Requires **Go 1.27+**. All dependencies are pure Go (no cgo), so cross-compiling
is trivial.

```bash
git clone https://github.com/all-thoughts-are-broken/xxxx.git
cd xxxx
go build -trimpath -ldflags "-s -w" -o bin/xxxx ./cmd    # Linux / macOS
go build -trimpath -ldflags "-s -w" -o bin/xxxx.exe ./cmd  # Windows
```

## Quick start

No code needed — pipe JSON in:

```bash
echo '{"cmd":"get_album_detail","task_id":1,"params":{"id":1472136}}' | ./bin/xxxx
echo '{"cmd":"commands"}'  | ./bin/xxxx    # list supported commands
echo '{"cmd":"shutdown"}'  | ./bin/xxxx    # replies "bye", then exits
```

The process emits a `ready` event on `stdout` before accepting requests.

## Protocol in one screen

**Request:** `{"cmd":"<name>","task_id":<any>,"params":{…}}`

**Success:** `{"task_id":1,"result":{…}}`
**Failure:** `{"task_id":1,"error":"not_found: 作品 999 不存在","error_code":"not_found"}`

**Progress:** `{"event":"progress","data":{"task_id":1,"stage":"download","done":12,"total":68}}`

Four rules the host must honour:

1. **Event frames carry no top-level `task_id`** — the host distinguishes
   responses from events by `'task_id' in msg`. Putting it on a progress event
   resolves the pending promise too early.
2. **`error` is a plain string** so the host can do `new Error(msg.error)`;
   branch on `error_code` instead.
3. **`shutdown` replies `"bye"` first, then exits.**
4. **Response order is not guaranteed** (up to 8 commands run concurrently) —
   match by `task_id`.

Error codes: `bad_request`, `unknown_command`, `invalid_params`, `not_found`,
`network`, `api`, `decrypt`, `io`, `canceled`, `timeout`, `panic`, `internal`.

## Commands

| Group | Commands |
|---|---|
| Built-in | `commands`, `cancel`, `shutdown` |
| Config | `config_get`, `config_set`, `probe` |
| Query | `get_album_detail`, `get_album_comment`, `get_comic_read` |
| Download | `download_album` (download → descramble → PDF) |
| Local | `restore_images`, `convert_images_to_PDF`, `merge_pdf`, `encrypt_pdf` |
| Render | `render_album_detail_image`, `render_album_comment_image` |

See the [Chinese README](README.md#3-命令参考) for every parameter and result shape.

## Configuration

Resolved in order: `$JM_CONFIG` → `config.json` next to the executable →
`config.json` in the working directory. A missing file is not an error (built-in
defaults apply). Start from [`config.example.json`](config.example.json) — the
real file is parsed as strict JSON, so it must not contain comments.

| Key | Default | Meaning |
|---|---|---|
| `proxy` | `""` | `http` / `https` / `socks5`; empty = direct |
| `base_url` | `""` | API endpoint; empty = auto-probe on start |
| `cdn_host` | `""` | image CDN; empty = learned from the response |
| `download_dir` / `output_dir` | `download` / `output` | raw / restored output roots |
| `workers` | `4` | concurrency |
| `max_retries` | `3` | per-file download retries |
| `skip_existing` | `true` | resume support |
| `pdf_max_page_height` | `0` | `0` = unlimited (one tall page per chapter) |
| `pdf_layout` | `single` | `single` or `paged` |
| `pdf_per_image_bookmark` | `true` | per-page bookmarks |
| `font_regular` / `font_bold` | `fonts/…` | CJK fonts for rendering |

The endpoint is deliberately **not** persisted: endpoints go stale within months,
and a saved value would short-circuit probing forever.

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

CI runs exactly these. The implementation-notes section of the Chinese README
explains the non-obvious constraints — **each one is backed by a test**, so if a
change conflicts with one, say why it no longer holds and update both the test
and the docs.

Releases: pushing a `v*` tag triggers `release.yml`, which cross-compiles five
targets on a Linux runner, packages binaries with `fonts/`, `config.example.json`
and docs, writes SHA-256 checksums, and publishes the release.

## Third-party components

Source code is MIT. Dependencies:
[disintegration/imaging](https://github.com/disintegration/imaging) (MIT),
[fogleman/gg](https://github.com/fogleman/gg) (MIT),
[jung-kurt/gofpdf](https://github.com/jung-kurt/gofpdf) (MIT),
[pdfcpu](https://github.com/pdfcpu/pdfcpu) (Apache-2.0),
[golang.org/x/image](https://pkg.go.dev/golang.org/x/image) (BSD-3-Clause).

The font in `fonts/` is **Noto Sans SC**, licensed under the
[SIL Open Font License 1.1](fonts/LICENSE-OFL.txt) — redistributed unmodified and
**not** covered by this project's MIT licence. See `fonts/README.md`.

## Disclaimer

This is a **technical tool**: it implements generic network, image-processing and
PDF-layout capabilities. It does not provide, host or distribute any content.
Users are responsible for complying with applicable laws and the terms of service
of any service they interact with. The interfaces it talks to come from a public
client's traffic and may change or stop working at any time; no warranty of any
kind is given.

## License

[MIT](LICENSE) © 2026 all-thoughts-are-broken.
`fonts/` is an exception — see above.
