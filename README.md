# Xuva

**A self-hosted media server you actually own.** Browse, organise, and stream your library to TV-first apps — without an account, a vendor relay, or ads.

[![CI](https://github.com/EJM-Digital/Xuva/actions/workflows/security.yml/badge.svg?branch=main)](https://github.com/EJM-Digital/Xuva/actions/workflows/security.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

---

## What you get

- **Polished browse + playback** for movies and TV, served by a single Go binary
- **Direct play preferred, transcoding when needed** — every decision is explainable
- **Native clients** on Apple TV, iOS / iPadOS, and Android TV; a built-in web app for everywhere else
- **LAN-first** — auto-discovery via mDNS, plays without internet, no required cloud account
- **In-app upgrades** on Windows and Docker — prompt-free, atomic, with auto-rollback
- **MIT-licensed**, runs on a stock home server, keeps working if we disappear

## Install

### Windows

Download the MSI from the [latest release](https://github.com/EJM-Digital/Xuva/releases/latest) and double-click. The installer registers the engine, the SYSTEM service, ffmpeg, firewall rules, and the auto-updater task in one run.

```powershell
# Or silent install (engine only, no auto-update):
msiexec /i xuva-server-v1.0.0.msi /qn ADDLOCAL=EngineFeature XUVA_AUTO_UPDATE=0
```

The data directory defaults to `C:\ProgramData\Xuva`; override with `XUVA_RUNTIME_HOME`.

### Docker

```bash
docker run -d --name xuva \
  -p 8097:8097 \
  -v xuva_data:/data \
  -v /path/to/movies:/movies:ro \
  -v /path/to/tv:/tv:ro \
  -e XUVA_MOVIES_PATH=/movies \
  -e XUVA_TV_PATH=/tv \
  -e PUID=1000 -e PGID=1000 \
  ghcr.io/ejm-digital/xuva:latest
```

Or use the bundled `docker-compose.yml` as a starting point (includes security hardening, log rotation, and resource-limit examples).

### Linux / macOS (from source)

```bash
git clone https://github.com/EJM-Digital/Xuva && cd Xuva/apps/web/svelte
npm ci && npm run publish:go-static
cd ../../../server
go build -o xuva ./cmd/Xuva
./xuva
```

The web UI is at <http://localhost:8097>.

## Clients

| Platform        | App                                                    | Status |
|-----------------|--------------------------------------------------------|--------|
| Web (any browser) | Built-in, served by the server                       | Stable |
| Windows desktop | MSI installer ships a tray app + the server           | Stable |
| Apple TV (tvOS) | [`apps/tvos`](apps/tvos)                              | Stable |
| iOS / iPadOS    | [`apps/ios`](apps/ios)                                | Stable |
| Android TV      | [`apps/android-tv`](apps/android-tv)                  | Beta   |

The web app is the easiest first launch. Pair native clients from inside the app (Settings → Devices) or scan the QR code on the TV.

## How it works

1. **Scan** — `XUVA_MOVIES_PATH` and `XUVA_TV_PATH` are walked, files are matched to TMDB/TVDB, and metadata is enriched.
2. **Probe** — ffprobe extracts streams, codecs, HDR, framerate, and language tags so playback decisions are deterministic.
3. **Decide** — the client sends a capability profile; the server picks direct play → remux → audio-only transcode → subtitle convert → full transcode (in that order) and tells you which path it took and why.
4. **Stream** — HLS or direct, with subtitle conversion, thumbnail scrubbing, resume, and per-profile parental ceilings.

## Documentation

- [Architecture](docs/architecture.md) — server layout, packages, data flow
- [Playback Engine](docs/playback-engine.md) — direct play vs transcode decision logic
- [Media Scanner](docs/media-scanner.md) — how libraries are indexed
- [Operations Runbook](docs/operations-runbook.md) — health checks, logs, backup
- [Release Versioning](docs/release-versioning.md) — semver policy
- [Security Policy](SECURITY.md) — how to report a vulnerability

## Contributing

Issues and discussions are welcome. For substantive changes, open an issue first so we can align on scope. Pull requests must pass the full CI suite (Go tests, Svelte type-check + Vitest, Docker smoke, Windows MSI build, Android build when touched, Apple build when touched).

## License

MIT — see [LICENSE](LICENSE).
