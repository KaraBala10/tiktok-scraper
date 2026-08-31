<div align="center">
  <img src="assets/logo.png" alt="tiktok-scraper" width="160" height="160" />
  <h1>tiktok-scraper</h1>
  <p>
    HTTP scraper that lists every video URL on a TikTok profile.<br />
    No browser. Stdlib only. Prints each URL as soon as it arrives.
  </p>
  <p>
    <a href="https://go.dev/doc/go1.23"><img src="https://img.shields.io/badge/Go-1.23+-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go 1.23+" /></a>
    <a href="https://go.dev/dl/"><img src="https://img.shields.io/badge/toolchain-go.dev%2Fdl-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go downloads" /></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-a3e635?style=for-the-badge&logo=opensourceinitiative&logoColor=white" alt="MIT License" /></a>
    <a href="CHANGELOG.md"><img src="https://img.shields.io/badge/changelog-Keep%20a%20Changelog-E05735?style=for-the-badge&logo=keepachangelog&logoColor=white" alt="Changelog" /></a>
    <a href="https://semver.org/spec/v2.0.0.html"><img src="https://img.shields.io/badge/versioning-SemVer-3F9FD7?style=for-the-badge&logo=semver&logoColor=white" alt="Semantic Versioning" /></a>
    <a href="go.mod"><img src="https://img.shields.io/badge/dependencies-stdlib-111827?style=for-the-badge&logo=go&logoColor=00ADD8" alt="Standard library only" /></a>
    <img src="https://img.shields.io/badge/platform-linux%20%7C%20macOS%20%7C%20Windows-111827?style=for-the-badge&logo=gnubash&logoColor=white" alt="linux, macOS, Windows" />
  </p>
</div>

---

## Table of contents

- [Install](#install)
- [Usage](#usage)
- [Output](#output)
- [How it works](#how-it-works)
- [Cache](#cache)
- [Versions](#versions)
- [Limits](#limits)
- [License](#license)

## Install

Requires [**Go 1.23+**](https://go.dev/doc/go1.23). Install a toolchain from [go.dev/dl](https://go.dev/dl/).

```bash
git clone https://github.com/KaraBala10/tiktok-scraper.git
cd tiktok-scraper
go build -o tiktok_scraper .
```

From the current directory if you already have the source:

```bash
go build -o tiktok_scraper .
```

The binary uses the [Go standard library](https://pkg.go.dev/std) only. No `go get` of third-party modules.

| Toolchain | Docs |
| --- | --- |
| Go 1.23 language notes | [go.dev/doc/go1.23](https://go.dev/doc/go1.23) |
| Go 1.23.0 tag | [github.com/golang/go/releases/tag/go1.23.0](https://github.com/golang/go/releases/tag/go1.23.0) |
| Module file | [`go.mod`](go.mod) (`go 1.23`) |

## Usage

Pass a username, an `@username`, or a profile/video URL:

```bash
./tiktok_scraper user2458298226194
./tiktok_scraper @user2458298226194
./tiktok_scraper https://www.tiktok.com/@user2458298226194
```

`m.tiktok.com` links and URLs without a scheme are accepted. A video URL is enough; the username is taken from the path.

```bash
./tiktok_scraper -limit 10 @user2458298226194
./tiktok_scraper -v https://www.tiktok.com/@user2458298226194
./tiktok_scraper @user2458298226194 > urls.txt
```

### Options

| Flag | Default | Description |
| --- | --- | --- |
| `-limit N` | `0` | Stop after `N` videos. `0` means the full profile. |
| `-v` | off | Write per-request timings to stderr. |

Run with no arguments to print usage.

## Output

```text
1. https://www.tiktok.com/@user/video/7679367388411743520
2. https://www.tiktok.com/@user/video/7679012345678901234
```

| Stream | Content |
| --- | --- |
| stdout | One numbered URL per line, flushed as they arrive |
| stderr | Errors, and timings when `-v` is set |

Exit status is `0` on success. It is `1` if the profile has no videos or a request fails before any URL is printed.

## How it works

TikTok's HTML profile page is not usable without a browser (WAF). The tool uses endpoints that still return data over plain HTTP:

1. `GET /embed/@user` for the first video IDs. Skipped when `secUid` is already cached.
2. `GET /embed/v2/{videoId}` to read `secUid`.
3. `GET /api/creator/item_list/` with `count=15` (the API rejects a larger count) and a millisecond cursor.

The first page of `item_list` is fetched on a dedicated connection. Further pages are requested in parallel: workers guess older cursors from video timestamps, then follow the exact next cursor when a page returns. Duplicate IDs are dropped. The loop stops when the API reports no older items.

A typical full-profile run is a few seconds, dominated by TikTok's time-to-first-byte (often ~1.4s on the first request).

## Cache

Files under `~/.cache/tiktok_scraper/` (or `$TMPDIR/tiktok_scraper/` if the user cache dir is unavailable):

| File | Purpose |
| --- | --- |
| `secuid.json` | Maps username to `secUid`, so later runs skip embed |
| `tls-session.json` | TLS session tickets for faster handshakes |

Delete that directory to start clean.

## Versions

This project follows [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html). User-facing changes are listed in [`CHANGELOG.md`](CHANGELOG.md) using [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

| Resource | Link |
| --- | --- |
| Changelog | [CHANGELOG.md](CHANGELOG.md) |
| License | [LICENSE](LICENSE) (MIT, 2026) |
| Go language version | [1.23 release notes](https://go.dev/doc/go1.23) |
| Go toolchains | [go.dev/dl](https://go.dev/dl/) |
| GitHub Releases | [KaraBala10/tiktok-scraper/releases](https://github.com/KaraBala10/tiktok-scraper/releases) |

Tag a release after the GitHub repo exists:

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The next published tag becomes the GitHub Release that the version badge tracks.

## Limits

- Unofficial APIs. TikTok can change, throttle, or block them.
- Public profiles only.
- Lists URLs. It does not download video files.
- At most 15 videos per API page, which is a server-side cap.

## License

[MIT](LICENSE) © 2026 [KaraBala](https://github.com/KaraBala10)
