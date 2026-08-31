# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and version numbers follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
