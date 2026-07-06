# RecordPal (may change)

An always-on, zero-interaction music identification appliance. Sits next to a record player or radio, listens continuously, and displays the current track, artist, album art, and metadata on a dedicated screen. No tapping. No phones.

---

## Architecture

The system is split into three components in a **Producer → Consumer → UI** pipeline, communicating over local interfaces on a single device.

```
Microphone → [audio-worker (Rust)] → Unix Socket → [now-playing-service (Go)] → SSE → [ui (React)]
```

**Why three components?** The audio capture layer is timing-sensitive and close to hardware. Keeping it isolated from the business logic (API calls, caching, state management) means a crash or restart in one doesn't affect the other. The UI layer is kept entirely separate so it can be developed and iterated on independently without touching the backend.

---

## Components

### `audio/` — Audio Worker (Rust)

The "ears" of the system. A lightweight native process responsible only for capturing audio and sending short WAV clips.

**Responsibilities:**
- Captures raw PCM audio from the default microphone via `cpal` (ALSA on Linux, CoreAudio on macOS)
- Records ~12-second clips in a continuous loop
- Calculates RMS amplitude and skips silent windows to avoid wasted API calls
- Frames a newline-terminated JSON header + raw WAV bytes over a Unix socket
- Reconnects automatically when the Go service restarts

**Key crates:** `cpal`, `hound`, `json`

---

### `middleware/` — Now Playing Service (Go)

The "brain" of the system. Handles identification, track state, and real-time updates to the UI.

**Responsibilities:**
- Listens on a Unix socket for framed audio clips (JSON header + WAV bytes)
- Sends clips to [AudD](https://audd.io/) for music identification
- Maps AudD metadata into a `Track` model, preferring Spotify album art and filtering out various-artists compilations; falls back to MusicBrainz studio-album releases when needed
- Debounces results: broadcasts a new track only when the title/artist changes; otherwise resets the idle timer
- Broadcasts `Track` and `IdentifierState` events over SSE (`GET /tracks`, `GET /state`)
- Returns to `idle` after 30 seconds with no incoming audio

**Key packages:** `net/http`, `github.com/AudDMusic/audd-go`, `github.com/joho/godotenv`

**Tests:** `go test ./...` covers track mapping, IPC framing, and the SSE broker.

---

### `ui/` — Frontend (React)

A full-screen kiosk-style interface. Connects to the Go service via Server-Sent Events and renders the current track in real time.

**Responsibilities:**
- Displays track title, artist, album, release year, and album art on a dark analog-inspired layout
- Reflects backend state (`listening`, `identifying`, `idle`, `error`) with animated feedback
- Self-hosted fonts via `@fontsource` (no Google Fonts CDN dependency)

During local development the UI is served by Vite. On the Pi, Chromium loads the built static bundle from disk and opens SSE connections to the Go service on `localhost:8080`.

---

## IPC Protocol

Components communicate over a Unix Domain Socket at `/tmp/nowplaying.sock` using a framed protocol: a single newline-terminated JSON header describing the clip, immediately followed by the raw audio bytes.

A text header keeps the metadata easy to inspect and evolve without a schema compiler, while sending the audio as raw bytes (rather than base64) avoids ~33% encoding overhead on every clip. The header's `bytes` field tells the reader exactly how many bytes of audio follow, so the Go side reads the header line and then reads that many bytes with `io.ReadFull`.

Each clip is sent as a JSON header line:

```json
{"type": "clip", "format": "wav", "duration_s": 12, "rms_energy": 0.045, "bytes": 320044}
```

…followed immediately by `bytes` bytes of WAV audio (header + PCM).

The Go service responds by broadcasting SSE events to connected UI clients.

---

## Data Flow

```
1. Capture     Microphone → cpal → ~12s WAV clip on disk
2. Filter      RMS check → drop if silent, pass if audio detected
3. Transport   Rust frames JSON header + WAV bytes → Unix socket
4. Ingest      Go reads the header, then the WAV bytes
5. Identify    Go sends the clip to AudD, gets the matched track
6. Enrich      Spotify album art + MusicBrainz studio-album fallback;
               skip various-artists compilations
7. Logic       Same track as current? → reset idle timer
               New track?             → broadcast over SSE
8. Display     React UI updates via /tracks and /state
```

---

## Local Development

**Prerequisites:** Go, Rust, Node/npm, an `AUDD_API_KEY`, and a microphone.

```bash
cp middleware/.env.example middleware/.env   # add your AUDD_API_KEY
make install                                 # fetch deps (one-time)
make dev                                     # middleware + Vite UI + audio worker
```

Other useful targets:

```bash
make dev-noaudio    # middleware + UI only (no mic / no AudD calls)
make test           # run Go tests
make build          # native build of all three components
make help           # full target list
```

To send a single pre-recorded clip without the mic: `cd audio && cargo run -- sample.wav`

---

## Persistence (planned)

SQLite caching for offline resilience and play history is planned but not implemented yet. Today the service is stateless aside from the current in-memory track and SSE subscribers.

---

## Hardware & Deployment

**Target hardware:** Raspberry Pi Zero 2 W or Pi 4  
**OS:** Raspberry Pi OS Lite (headless) + a display stack for Chromium kiosk mode  
**Display:** Any HDMI screen; Chromium runs in kiosk mode

### Build & deploy

Local development uses `Makefile`. Cross-compilation and Pi deployment use `Makefile.pi` (a scaffold — not yet validated on real hardware):

```bash
make -f Makefile.pi build              # cross-compile Go + Rust + UI into dist/pi
make -f Makefile.pi fetch-wifi-connect # download balena wifi-connect (arm64)
make -f Makefile.pi deploy             # rsync to pi@recordpal.local, install units, restart
```

Rust cross-compilation defaults to [`cross`](https://github.com/cross-rs/cross) (Docker-based). Override targets as needed, e.g. `make -f Makefile.pi deploy PI_HOST=192.168.1.42`.

Place `AUDD_API_KEY=...` in `/opt/recordpal/.env` on the device before starting services.

### Services (systemd)

Four systemd units in `deploy/` manage the processes on the device:

| Unit | Description |
|------|-------------|
| `wifi-connect.service` | Balena wifi-connect. Opens a WiFi setup portal when offline; exits once connected. Runs before the app units. |
| `now-playing.service` | Go service. Owns the socket file. Starts after networking is up. |
| `audio-worker.service` | Rust binary. Depends on `now-playing.service`. |
| `kiosk.service` | Chromium in kiosk mode, loading the built UI from `/opt/recordpal/ui`. |

```bash
sudo systemctl start now-playing audio-worker kiosk
make -f Makefile.pi logs   # tail all service logs over SSH
```

### Network provisioning

On first boot — or any time no known WiFi is in range — the Pi needs a way to receive WiFi credentials without a keyboard attached. We use [balena wifi-connect](https://github.com/balena-os/wifi-connect), a standalone binary that brings up a temporary `RecordPal-Setup` access point and serves a captive portal: connect a phone, pick your network, enter the password, and the Pi saves it via NetworkManager and reconnects automatically on every subsequent boot.

wifi-connect is a prebuilt tool we run as-is, not something we compile — `make -f Makefile.pi fetch-wifi-connect` downloads the `aarch64` release and `deploy` installs it alongside our own binaries.

> **Future:** wifi-connect is a pragmatic starting point. Down the line we may replace it with our own Go implementation — NetworkManager over D-Bus via [`gonetworkmanager`](https://github.com/Wifx/gonetworkmanager) plus a portal served by the existing `net/http` stack — so the setup screen matches RecordPal's look and we drop the external dependency.

---

## Repo Structure

```
recordPal/
├── audio/           # Rust — audio capture and clip transmission
├── middleware/      # Go — identification service and SSE endpoints
├── ui/              # React — kiosk display
├── deploy/          # systemd unit files
├── Makefile         # local dev: build, test, run
├── Makefile.pi      # Pi cross-compile + deploy scaffold
└── README.md
```

---

## Privacy

Audio is captured on-device and a short clip is sent over an encrypted connection to AudD solely to identify the track. Per their policy, the clip is fingerprinted and then permanently discarded — only the derived fingerprint (which contains no recoverable audio) is retained on their side. No audio is stored on the device.
