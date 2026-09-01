# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and version numbers follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.1] - 2026-09-01

### Fixed

- Explore session region now follows the current IP instead of printing `?` on mint and then saving the `-region` default (`KW`)
- Look up the IP country with ip-api; Cloudflare `loc` mislabels some VPN exits (e.g. Kuwait as `FR`)
- Treat HTTP 451 as a country ban: keep the saved session and do not remint; an `msToken` cannot bypass TikTok geo-blocks
- Explore paginates with the API `cursor` instead of repeating the same first page
- After the comedy feed ends, mint a new session and keep paging past already-seen videos; do not remint on empty/rate-limit pages

## [0.3.0] - 2026-09-01

### Added

- Explore mode (`-explore`) that lists TikTok web Comedy chip videos (`categoryType=104`) until Ctrl+C, or `-limit N`
- Automatic web `msToken` mint via Chrome TLS impersonation when no session exists; `-refresh` forces a new token
- `--help` listing profile and explore flags
- Explore session files under `~/.cache/tiktok_scraper/` (`session.json`, `cookies.txt`, `device_id.txt`)

### Changed

- Split the CLI into `internal/profile` (original scraper) and `internal/explore` (web comedy)
- Explore follows the current IP for geo; `-region` only fills the query pack
- Bump `tls-client` to v1.9.2, `fhttp` to v0.5.36, and Go 1.23-compatible indirect modules

### Removed

- App Explore fallback (unsigned mobile feed). Explore is web-only

## [0.2.0] - 2026-09-01

### Changed

- Adaptive speculative paging: per-user posting-cadence and page-boundary marks cached under `~/.cache/tiktok_scraper/cadence.json`, so repeat runs prefetch deep pages in parallel with page 1
- Request throttle with pacing and cooldown; retry on HTTP 429 / 403 / status 10102 / empty pages and keep paging instead of stopping the profile early
- Pin the last working IP per host in `~/.cache/tiktok_scraper/dns.json` and dial it directly, so a resolver hiccup no longer aborts a run; a stale pin falls back to DNS and heals itself
- Event-driven page pipeline wakeup instead of 2 ms polling
- Stream `secUid` extraction with early exit instead of buffering the whole embed page
- Batched stdout flushes in the URL printer
- TLS session cache is flushed synchronously on exit

### Removed

- `warmTLS` favicon pre-warm request (it always raced and lost against the first real request)

## [0.1.1] - 2026-08-31

### Changed

- Consume overlapping worker pages while waiting for the next `item_list` cursor
- Debounce TLS session cache writes
- Larger HTTP buffers and a longer TCP dial timeout

## [0.1.0] - 2026-08-31

### Added

- HTTP-only profile scraper (no browser)
- Parallel `item_list` paging with guessed cursors
- Username, `@username`, and profile/video URL input
- `secUid` and TLS session cache under `~/.cache/tiktok_scraper/`
- `-limit` and `-v` flags
- Linux x86_64 and Windows x86_64 release binaries
