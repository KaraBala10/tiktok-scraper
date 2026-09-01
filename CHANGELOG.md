# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and version numbers follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
