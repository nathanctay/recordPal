# Shazablet

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

The "ears" of the system. A lightweight native process responsible only for capturing audio and producing fingerprints.

**Responsibilities:**
- Captures raw PCM audio via ALSA
- Maintains a ring buffer to decouple capture rate from processing rate
- Calculates RMS amplitude; skips silent windows to avoid wasted CPU and API calls
- Generates Chromaprint acoustic fingerprints from valid audio windows (6–10 seconds)
- Serializes fingerprint data as NDJSON and writes to a Unix Domain Socket
- Reconnects automatically if the Go service restarts

**Key crates:** `cpal` (cross-platform audio I/O), `rustfft`, `chromaprint` bindings, `serde_json`, `tracing`

---

### `middleware/` — Now Playing Service (Go)

The "brain" of the system. Handles all application logic, external API calls, persistence, and serves the frontend.

**Responsibilities:**
- Listens on a Unix Domain Socket for the NDJSON stream from the audio worker
- Debounces: if the incoming fingerprint matches the current track, updates a `last_heard` timestamp and skips API lookups
- Queries AcoustID / MusicBrainz / Spotify only when a genuinely new track is detected
- Caches results in SQLite to avoid re-querying known tracks and to survive network outages
- Manages a TTL: if no audio is detected for 5 minutes, broadcasts an `idle` event to the UI
- Hosts the frontend static files and provides an SSE endpoint for real-time UI updates

**Key packages:** `net/http` (stdlib router), `modernc.org/sqlite`, `encoding/json`

---

### `ui/` — Frontend (React)

A full-screen kiosk interface running in Chromium on the device. Connects to the Go service via Server-Sent Events and renders the current track in real time.

**Responsibilities:**
- Displays track title, artist, album name, and album art
- Transitions to an idle/clock state when the Go service broadcasts `status: idle`
- No user interaction required — purely a display layer

---

## IPC Protocol

Components communicate using **NDJSON (Newline Delimited JSON)** over a Unix Domain Socket at `/tmp/nowplaying.sock`.

NDJSON was chosen over binary protocols (protobuf, msgpack) because it is trivially debuggable with standard tools like `nc` and `cat`, requires no build steps or schema compilation, and is lightweight enough for this use case.

Each message from the audio worker is one JSON object per line:

```json
{"type": "fingerprint", "payload": "AQAAz0mUCJGi...", "duration_ms": 7500, "rms_energy": 0.045}
```

The Go service responds by broadcasting SSE events to connected UI clients.

---

## Data Flow

```
1. Capture    Microphone → ALSA → Rust ring buffer
2. Filter     RMS check → drop if silent, pass if audio detected
3. Fingerprint Chromaprint generates fingerprint string
4. Transport  Rust serializes to NDJSON → writes to /tmp/nowplaying.sock
5. Ingest     Go service decodes stream
6. Logic      Same song as last 10s? → reset idle timer only
              New song?              → check SQLite cache → (miss) call API → update DB
7. Broadcast  Go SSE endpoint pushes track data to React UI
```

---

## Persistence

**Database:** SQLite (`nowplaying.db`)

| Table | Columns |
|-------|---------|
| `tracks` | `id`, `title`, `artist`, `album`, `album_art_url`, `fingerprint_hash` |
| `history` | `id`, `track_id`, `first_heard`, `last_heard` |
| `kv_store` | `key`, `value` — API keys, config tokens |

The local cache prevents redundant API calls when the same track is heard again (e.g. replaying the same album). History is stored for future features like a "recently played" view.

---

## Hardware & Deployment

**Target hardware:** Raspberry Pi Zero 2 W or Pi 4  
**OS:** Raspberry Pi OS Lite (headless)  
**Display:** Any HDMI screen; Chromium runs in kiosk mode

### Build

Builds are handled by a Dockerized cross-compilation pipeline targeting `linux/arm64`, so no ARM toolchain needs to be set up on your development machine.

```bash
make build-all      # cross-compile Rust + Go for linux/arm64
make deploy         # rsync binaries + assets to Pi over SSH
```

### Services (systemd)

Three systemd units manage the processes on the device:

| Unit | Description |
|------|-------------|
| `now-playing.service` | Go service. Owns the socket file. Starts first. |
| `audio-worker.service` | Rust binary. Depends on `now-playing.service`. |
| `kiosk.service` | Starts X11/Wayland + Chromium in kiosk mode. |

```bash
sudo systemctl start now-playing audio-worker kiosk
sudo journalctl -u audio-worker -f   # tail logs for any unit
```

---

## Repo Structure

```
shazablet/
├── audio/          # Rust — audio capture and fingerprinting
├── middleware/      # Go — identification service and web server
├── ui/             # React — kiosk display
├── deploy/         # systemd unit files, install scripts
├── docs/           # architecture notes, API references
├── Makefile        # build-all, deploy, restart-*, logs-*
└── README.md
```

---

## Development Roadmap

### Phase 1 — Pipeline (PoC)
- [ ] Rust captures audio and prints NDJSON fingerprints to stdout
- [ ] Go reads stdin and logs "fingerprint received"
- [ ] Verify end-to-end data shape before wiring anything together

### Phase 2 — Socket & Identification
- [ ] Connect Rust → Go via Unix Domain Socket
- [ ] Implement AcoustID / MusicBrainz lookup in Go
- [ ] Basic SSE endpoint returning raw JSON to a browser

### Phase 3 — UI & Persistence
- [ ] SQLite caching layer
- [ ] React frontend: track display, album art, idle state
- [ ] Chromium kiosk setup on Pi

### Phase 4 — Hardening
- [ ] Tune RMS silence threshold for real-world room noise
- [ ] Idle TTL logic and clock/screensaver state
- [ ] Handle network outages gracefully (queue or serve from cache)
- [ ] Dockerized cross-compile + deploy pipeline

---

## Privacy

Audio is processed entirely on-device. Raw audio is never transmitted anywhere. Only the generated fingerprint string (a compact numeric hash with no recoverable audio content) is sent to external identification APIs.
