# swoop

IDM-style multi-connection downloads in your terminal. Run `swoop`, paste a link, press Enter.

```
  ███████╗██╗    ██╗ ██████╗  ██████╗ ██████╗
  ██╔════╝██║    ██║██╔═══██╗██╔═══██╗██╔══██╗
  ███████╗██║ █╗ ██║██║   ██║██║   ██║██████╔╝
  ╚════██║██║███╗██║██║   ██║██║   ██║██╔═══╝
  ███████║╚███╔███╔╝╚██████╔╝╚██████╔╝██║
  ╚══════╝ ╚══╝╚══╝  ╚═════╝  ╚═════╝ ╚═╝

❯ https://example.com/ubuntu-24.04.iso
  ↳ ubuntu-24.04.iso · 4.70 GB · 16 connections
╭─ ubuntu-24.04.iso ─────────────────────────────────────────────────╮
│ ███████████████████████████████▌░░░░░░░░░░░░░░░░░░░░░░░░    55.0% │
│ 2.58 GB / 4.70 GB   ↓ 118.20 MB/s   ETA 18s                        │
│ 16 conns · 3 splits · 0 retries · 22s elapsed                      │
│                                                                    │
│ ██████▓░░░██████▓░░░██████▓░░░██████▓░░░██████▓░░░██████▓░░░██████ │
│ ██████▓░░░██████▓░░░██████▓░░░██████▓░░░██████▓░░░██████▓░░░██████ │
╰────────────────────────────────────────────────────────────────────╯
```

Each connection draws its finished bytes in its own color, and `▓` is where it is reading right now.

## Install

**macOS / Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/prashant-zo/swoop/main/install.sh | sh
```

**Windows** (PowerShell)

```powershell
irm https://raw.githubusercontent.com/prashant-zo/swoop/main/install.ps1 | iex
```

from cmd.exe:

```bat
powershell -c "irm https://raw.githubusercontent.com/prashant-zo/swoop/main/install.ps1 | iex"
```

**Any OS with Go**

```sh
go install github.com/prashant-zo/swoop@latest
```

Builds for macOS, Linux and Windows on arm64 and amd64. Single binary, no dependencies. Both installers check the sha256 checksum before installing. CI runs the full test suite, including real end-to-end downloads, on all three OSes.

Colors: swoop uses 24-bit color when the terminal supports it (iTerm2, Windows Terminal, WezTerm, Ghostty, VS Code) and falls back to 256 colors elsewhere, like macOS Terminal. Force a mode with `SWOOP_COLOR=truecolor` or `SWOOP_COLOR=256`, or turn colors off with `NO_COLOR=1`.

## Use

```sh
swoop                                # interactive: paste a link, press Enter
swoop https://example.com/file.iso   # download right away
swoop -c 32 -o ~/Movies URL          # 32 connections, custom folder
swoop -H "Cookie: session=abc" URL   # extra headers for links that need login
```

Inside the interactive UI:

| key | does |
|---|---|
| paste + Enter | download (several links separated by spaces are queued) |
| Enter on empty line | grab the link in your clipboard |
| `open` | open the download folder |
| Ctrl+C | pause the current download, paste the same link to resume |
| `q` | quit |

## How it works

- **Probe** with `GET Range: bytes=0-0` instead of HEAD, because S3 presigned URLs and many CDNs reject HEAD.
- **Split** the file into up to 16 byte ranges (max 32), each on its own TCP connection. HTTP/1.1 is forced on purpose: with HTTP/2, Go would put every range on one socket and the parallelism would be fake.
- **Dynamic re-splitting.** When a connection finishes its piece, it takes the second half of the biggest piece still left. A slow connection can't hold up the end of the download.
- **Direct writes.** Every connection writes with `WriteAt` straight into one pre-allocated `.part` file, so there's no merge step at the end.
- **Resume.** State is saved every second next to the `.part` file. Ctrl+C, a network drop or a crash costs at most about one second of work.
- **Retries.** Each connection retries with backoff, a 15 second stall watchdog reconnects dead sockets, and extra connections step back when a server answers 429 or 503.
- **Fallback.** If the server doesn't support ranges, swoop downloads it as one stream.
- **Safe names.** File names from `Content-Disposition` are sanitized, so a server can't write outside your download folder.

## Benchmarks

Local test server that caps every connection at 2 MB/s (a common way real servers throttle), 64 MB file, sha256 verified on every run:

| tool | time |
|---|---|
| curl (one stream) | 32.5 s |
| swoop `-c 5` | 6.6 s |
| swoop `-c 16` | 2.1 s |
| swoop `-c 32` | 1.1 s |

Same file, 8 connections, one in every four connections 8x slower:

| | time |
|---|---|
| static split (no re-splitting) | 32.5 s |
| swoop (dynamic re-splitting) | 11.3 s |

## Honest limits

- If one connection already fills your internet line, swoop ties with curl and the browser. It wins when the server throttles each connection or the server is far away.
- Servers without range support get a single stream.
- Some servers limit connections per IP. swoop backs off, but lower `-c` if a site complains.
- There's no browser extension, so it won't grab YouTube or streaming videos.

## Release

```sh
git tag v0.1.0 && git push origin v0.1.0
```

The GitHub Actions workflow tests the code, builds binaries for macOS, Linux and Windows, writes `checksums.txt` and publishes the release. `install.sh` and `install.ps1` download from the latest release and check the checksum.
