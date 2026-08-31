<div align="center">
  <img src="assets/logo.png" alt="tiktok-scraper" width="160" height="160" />
  <h1>tiktok-scraper</h1>
  <p>
    HTTP scraper that lists every video URL on a TikTok profile.<br />
    No browser. Stdlib only. Prints each URL as soon as it arrives.
  </p>
  <p>
    <a href="https://github.com/KaraBala10/tiktok-scraper/releases/latest"><img src="https://img.shields.io/github/v/release/KaraBala10/tiktok-scraper?style=for-the-badge&logo=github&label=latest%20release" alt="Latest release" /></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-a3e635?style=for-the-badge&logo=opensourceinitiative&logoColor=white" alt="MIT License" /></a>
    <a href="CHANGELOG.md"><img src="https://img.shields.io/badge/changelog-Keep%20a%20Changelog-E05735?style=for-the-badge&logo=keepachangelog&logoColor=white" alt="Changelog" /></a>
    <a href="https://semver.org/spec/v2.0.0.html"><img src="https://img.shields.io/badge/versioning-SemVer-3F9FD7?style=for-the-badge&logo=semver&logoColor=white" alt="Semantic Versioning" /></a>
    <img src="https://img.shields.io/badge/platform-Linux%20x86__64-111827?style=for-the-badge&logo=linux&logoColor=white" alt="Linux x86_64" />
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
- [Build from source](#build-from-source)

## Install

Download a ready binary from the latest GitHub Release. You do not need Go, and you do not need to clone this repository.

**[Latest release](https://github.com/KaraBala10/tiktok-scraper/releases/latest)** (always the current version)

On that page, under **Assets**, download the archive for your OS. Releases currently ship Linux x86_64. No extra libraries or language runtimes are required. A network connection is required so the tool can talk to TikTok.

```bash
tar -xzf tiktok-scraper-*-linux-amd64.tar.gz
chmod +x tiktok_scraper-linux-amd64
./tiktok_scraper-linux-amd64 @username
```

The exact filename changes with each version. Use the file listed on the [latest release](https://github.com/KaraBala10/tiktok-scraper/releases/latest) page.

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
| Latest release | [releases/latest](https://github.com/KaraBala10/tiktok-scraper/releases/latest) |
| All releases | [releases](https://github.com/KaraBala10/tiktok-scraper/releases) |
| Changelog | [CHANGELOG.md](CHANGELOG.md) |
| License | [LICENSE](LICENSE) (MIT, 2026) |

## Limits

- Unofficial APIs. TikTok can change, throttle, or block them.
- Public profiles only.
- Lists URLs. It does not download video files.
- At most 15 videos per API page, which is a server-side cap.

## License

[MIT](LICENSE) © 2026 [KaraBala](https://github.com/KaraBala10)

## Build from source

Only needed if you are changing the code. Users should install from the [latest release](https://github.com/KaraBala10/tiktok-scraper/releases/latest).

Requires [Go 1.23+](https://go.dev/doc/go1.23). No third-party modules.

```bash
git clone https://github.com/KaraBala10/tiktok-scraper.git
cd tiktok-scraper
go build -o tiktok_scraper .
```
