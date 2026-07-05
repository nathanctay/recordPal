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

The "ears" of the system. A lightweight native process responsible only for capturing audio and sending short audio clips.

**Responsibilities:**
- Captures raw PCM audio via ALSA
- Maintains a ring buffer to decouple capture rate from processing rate
- Calculates RMS amplitude; skips silent windows to avoid wasted CPU and API calls
- Encodes the captured window as a self-contained WAV clip (e.g. ~10s, mono 16 kHz 16-bit)
- Frames a small JSON header + the raw WAV bytes over the socket
- Reconnects automatically if the Go service restarts

**Key crates:** `cpal` (cross-platform audio I/O), `serde_json`, `tracing`, `hound`

---

### `middleware/` — Now Playing Service (Go)

The "brain" of the system. Handles all application logic, external API calls, persistence, and serves the frontend.

**Responsibilities:**
- Receives framed audio clips (JSON header + WAV bytes) from the audio worker
- Queries AudD (which returns Spotify/Apple Music/MusicBrainz metadata + album art in one call)
- Debounces: compares each AudD result to the current track, broadcasting only when the track changes — otherwise it just bumps `last_heard` and resets the idle timer
- Caches results in SQLite to survive network outages
- Manages a TTL: if no audio is detected for 5 minutes, broadcasts an `idle` event to the UI
- Hosts the frontend static files and provides an SSE endpoint for real-time UI updates

**Key packages:** `net/http` (stdlib router), `modernc.org/sqlite`, `encoding/json`, `mime/multipart`

---

### `ui/` — Frontend (React)

A full-screen kiosk interface running in Chromium on the device. Connects to the Go service via Server-Sent Events and renders the current track in real time.

**Responsibilities:**
- Displays track title, artist, album name, and album art
- Transitions to an idle/clock state when the Go service broadcasts `status: idle`
- No user interaction required — purely a display layer

---

## IPC Protocol

Components communicate over a Unix Domain Socket at `/tmp/nowplaying.sock` using a simple framed protocol: a single newline-terminated JSON header describing the clip, immediately followed by the raw audio bytes.

A text header keeps the metadata easy to inspect and evolve without a schema compiler, while sending the audio as raw bytes (rather than base64) avoids ~33% encoding overhead on every clip. The header's `bytes` field tells the reader exactly how many bytes of audio follow, so the Go side reads the header line and then reads that many bytes with `io.ReadFull`.

Each clip is sent as a JSON header line:

```json
{"type": "clip", "format": "wav", "duration_s": 10, "rms_energy": 0.045, "bytes": 320044}
```

…followed immediately by `bytes` bytes of WAV audio (header + PCM).

The Go service responds by broadcasting SSE events to connected UI clients.

---

## Data Flow

```
1. Capture     Microphone → ALSA → Rust ring buffer
2. Filter      RMS check → drop if silent, pass if audio detected
3. Encode      Wrap captured PCM window as a WAV clip
4. Transport   Rust frames JSON header + WAV bytes → socket
5. Ingest      Go reads the header, then the WAV bytes
6. Identify    Go sends the clip to AudD, gets the matched track
7. Logic       Same track as current? → bump last_heard, reset idle timer
               New track?             → update current, write history, broadcast
8. Broadcast   Go SSE endpoint pushes track data to React UI
```

---

## Persistence

**Database:** SQLite (`nowplaying.db`)

| Table | Columns |
|-------|---------|
| `tracks` | `id`, `title`, `artist`, `album`, `album_art_url` |
| `history` | `id`, `track_id`, `first_heard`, `last_heard` |

Recognized tracks and their metadata are cached so the current song and history stay available during network outages. Because a clip can only be identified by calling AudD, the cache is not used to skip identification requests — it exists for offline resilience and history (e.g. a future "recently played" view).

---

## Hardware & Deployment

**Target hardware:** Raspberry Pi Zero 2 W or Pi 4  
**OS:** Raspberry Pi OS Lite (headless)  
**Display:** Any HDMI screen; Chromium runs in kiosk mode

### Build

Builds are handled by a Dockerized cross-compilation pipeline targeting `linux/arm64`, so no ARM toolchain needs to be set up on your development machine.

```bash
make build-all      # cross-compile Rust + Go for linux/arm64
make deploy         # fetch wifi-connect + rsync binaries + assets to Pi over SSH
```

### Services (systemd)

Four systemd units manage the processes on the device:

| Unit | Description |
|------|-------------|
| `wifi-connect.service` | Balena wifi-connect. Opens a WiFi setup portal when offline; exits once connected. Runs before the app units. |
| `now-playing.service` | Go service. Owns the socket file. Starts after networking is up. |
| `audio-worker.service` | Rust binary. Depends on `now-playing.service`. |
| `kiosk.service` | Starts X11/Wayland + Chromium in kiosk mode. |

```bash
sudo systemctl start now-playing audio-worker kiosk
sudo journalctl -u audio-worker -f   # tail logs for any unit
```

### Network Provisioning

On first boot — or any time no known WiFi is in range — the Pi needs a way to receive WiFi credentials without a keyboard attached. We use [balena wifi-connect](https://github.com/balena-os/wifi-connect), a standalone binary that brings up a temporary `RecordPal-Setup` access point and serves a captive portal: connect a phone, pick your network, enter the password, and the Pi saves it via NetworkManager and reconnects automatically on every subsequent boot.

wifi-connect is a prebuilt tool we run as-is, not something we compile — `make deploy` downloads the `aarch64` release and installs it alongside our own binaries. It runs as the `wifi-connect.service` unit, which only opens the portal when the device is offline; once a connection succeeds it exits and the app services start.

> **Future:** wifi-connect is a pragmatic starting point. Down the line we may replace it with our own Go implementation — NetworkManager over D-Bus via [`gonetworkmanager`](https://github.com/Wifx/gonetworkmanager) plus a portal served by the existing `net/http` stack — so the setup screen matches RecordPal's look and we drop the external dependency.

---

## Repo Structure

```
recordPal/
├── audio/          # Rust — audio capture and clip transmission
├── middleware/      # Go — identification service and web server
├── ui/             # React — kiosk display
├── deploy/         # systemd unit files, install scripts
├── docs/           # architecture notes, API references
├── Makefile        # build-all, deploy, restart-*, logs-*
└── README.md
```

---

## Privacy

Audio is captured on-device and a short clip is sent over an encrypted connection to our music-identification provider (AudD) solely to identify the track. Per their policy, the clip is fingerprinted and then permanently discarded — only the derived fingerprint (which contains no recoverable audio) is retained on their side. No audio is stored on the device.
