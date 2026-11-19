# swoop

Fast downloads, right in your terminal. Paste a link, press Enter.

```
  ███████╗██╗    ██╗ ██████╗  ██████╗ ██████╗
  ██╔════╝██║    ██║██╔═══██╗██╔═══██╗██╔══██╗
  ███████╗██║ █╗ ██║██║   ██║██║   ██║██████╔╝
  ╚════██║██║███╗██║██║   ██║██║   ██║██╔═══╝
  ███████║╚███╔███╔╝╚██████╔╝╚██████╔╝██║
  ╚══════╝ ╚══╝╚══╝  ╚═════╝  ╚═════╝ ╚═╝

  fast downloads, right in your terminal   v0.2.0
  saves to ~/Downloads  ·  up to 16 connections per file
  paste a link and press Enter  ·  type help anytime

❯ https://example.com/ubuntu-24.04.iso
  ↳ ubuntu-24.04.iso · 4.70 GB · 16 connections
╭─ ubuntu-24.04.iso ───────────────────────────────────────────────────╮
│ ███████████████████████████▌░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░    48.6% │
│ 2.28 GB / 4.70 GB   ↓ 96.40 MB/s   ETA 25s                           │
│ 16 connections · 3 splits · 0 retries · 24s                          │
│                                                                      │
│ ██████▓░░░██████▓░░░██████▓░░░██████▓░░░██████▓░░░██████▓░░░██████▓░ │
│ ██████▓░░░██████▓░░░██████▓░░░██████▓░░░██████▓░░░██████▓░░░██████▓░ │
╰──────────────────────────────────────────────────────────────────────╯
  p pause   x cancel   Ctrl+C pause
```

Each connection draws its part of the file in its own color, and `▓` is where it's reading right now.

## Install

**Mac and Linux**: open Terminal and paste:

```sh
curl -fsSL https://raw.githubusercontent.com/prashant-zo/swoop/main/install.sh | sh
```

**Windows**: open PowerShell and paste:

```powershell
irm https://raw.githubusercontent.com/prashant-zo/swoop/main/install.ps1 | iex
```

Then type `swoop` and press Enter. That's all.

<details>
<summary>Other ways to install</summary>

From cmd.exe on Windows:

```bat
powershell -c "irm https://raw.githubusercontent.com/prashant-zo/swoop/main/install.ps1 | iex"
```

With Go:

```sh
go install github.com/prashant-zo/swoop@latest
```

Both installers check the file's sha256 checksum before installing. swoop is a single program with no dependencies, built for Mac, Linux and Windows (Intel and ARM).
</details>

## How to use it

1. Type `swoop` and press Enter.
2. Paste a link and press Enter.

If you just copied a link, press Enter on an empty line and swoop uses it.

### While a file downloads

| key | what it does |
|---|---|
| `p` | pause, you can continue later |
| `x` | cancel and delete the unfinished file (press it twice, so you can't do it by accident) |
| `Ctrl+C` | pause |

### Your downloads

Type `list` to see everything you downloaded.

| key | what it does |
|---|---|
| `↑` `↓` | choose a download |
| `Enter` | continue a paused download, or open a finished one |
| `o` | show it in Finder or Explorer |
| `d` | remove it from the list (the file stays on your computer) |
| `x` | remove it and delete the file too (asks first) |
| `Esc` | go back |

### Other commands

| command | what it does |
|---|---|
| `help` | show all keys and commands |
| `folder ~/Movies` | save new downloads to another folder |
| `open` | open the download folder |
| `q` | quit (paused downloads stay in your list) |

### From scripts

```sh
swoop https://example.com/file.iso          # download right away
swoop -c 32 -o ~/Movies LINK                # 32 connections, another folder
swoop -H "Cookie: session=abc" LINK         # links that need a login
swoop list                                  # open your downloads list
```

## Torrents

Paste a magnet link or a `.torrent` link, or drag a `.torrent` file into the window. swoop shows the same progress panel and the same keys.

Torrents use [aria2](https://aria2.github.io/), a free open-source download engine. Install it once:

| | |
|---|---|
| Mac | `brew install aria2` |
| Windows | `winget install aria2.aria2` |
| Linux | `sudo apt install aria2` |

If it's missing, swoop tells you what to run.

## Why it's fast

- **Many connections at once.** swoop splits a file into up to 16 parts (`-c 32` for more) and downloads them in parallel, each over its own connection. Many servers limit how fast one connection can go, and distant servers are slow on a single connection, so splitting helps a lot.
- **No slow finish.** When one connection finishes its part, it takes over half of the biggest part still left. One slow connection can't hold up the end of the download.
- **Real parallel connections.** HTTP/1.1 is used on purpose. With HTTP/2, all parts would share one connection and the parallelism would be fake.
- **Pause and continue.** Progress is saved every second next to the file. Pausing, closing the window, losing the network or even a crash costs about a second of work.
- **Retries on its own.** Dropped or stuck connections reconnect automatically, and swoop backs off politely when a server says it's busy.
- **Safe file names.** A server can't make swoop write outside your download folder.

## Benchmarks

Local test server that limits each connection to 2 MB/s (a common way servers throttle), 64 MB file, checked byte for byte on every run:

| tool | time |
|---|---|
| curl (one connection) | 32.5 s |
| swoop `-c 5` | 6.6 s |
| swoop `-c 16` | 2.1 s |
| swoop `-c 32` | 1.1 s |

Same file, 8 connections, with one in four connections running 8x slower:

| | time |
|---|---|
| fixed split (no re-splitting) | 32.5 s |
| swoop | 11.3 s |

## Good to know

- If one connection already fills your internet line, swoop is as fast as your browser, not faster. It shines with servers that throttle each connection and servers far away from you.
- Some servers don't allow split downloads. swoop then uses one connection.
- Some servers limit connections per person. If a site complains, use fewer, like `-c 4`.
- Only download things you have the right to download.

## Inspired by

swoop is inspired by Internet Download Manager (IDM) and the way it splits downloads across many connections. It's a separate project, built from scratch for the terminal.

AI was used to help write the code.

## Release

```sh
git tag v0.2.0 && git push origin v0.2.0
```

GitHub Actions runs the tests on Mac, Linux and Windows, builds all binaries, writes `checksums.txt` and publishes the release. The installers always fetch the latest release.

## License

MIT
