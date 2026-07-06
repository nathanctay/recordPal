package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	audd "github.com/AudDMusic/audd-go"
	"github.com/joho/godotenv"
)

var (
	errBadClipHeader       = errors.New("bad clip header")
	errImplausibleClipSize = errors.New("implausible clip size")
)

const maxClipBytes = 10 << 20 // 10 MB — AudD's hard limit

type ClipHeader struct {
	Type      string  `json:"type"`
	Format    string  `json:"format"`
	DurationS int     `json:"duration_s"` // clip length (~10s), informational
	RmsEnergy float32 `json:"rms_energy"` // informational
	Bytes     int     `json:"bytes"`      // length of the WAV payload that follows
}

type Track struct {
	Album         string                     `json:"album"`
	AlbumCoverURL string                     `json:"album_cover_url"`
	Artist        string                     `json:"artist"`
	DurationS     int                        `json:"duration_s"`
	Extras        map[string]json.RawMessage `json:"extras"`
	ISRC          string                     `json:"isrc,omitempty"`
	ReleaseDate   string                     `json:"release_date"`
	Title         string                     `json:"title"`
	// Progress int POTENTIALLY HAVE IT RETURN WHERE IN THE SONG THEY ARE TO DISPLAY SONG PROGRESS
	// Trivia []string
}

type IdentifierState string

const (
	StateIdle        IdentifierState = "idle"
	StateListening   IdentifierState = "listening"
	StateIdentifying IdentifierState = "identifying"
	StateError       IdentifierState = "error"
)

func init() {
	if err := godotenv.Load(); err != nil {
		log.Print("No .env file found")
	}
}

var audDClient *audd.Client

func main() {
	audDApiKey, ok := os.LookupEnv("AUDD_API_KEY")
	if !ok {
		log.Fatal("AUDD_API_KEY is not set")
	}

	// 20s is plenty for AudD (it usually answers in a few seconds) and keeps the
	// UI from stalling in "identifying" for a full minute when a request hangs.
	audDClient = audd.NewClient(audDApiKey, audd.WithStandardTimeout(20*time.Second))
	defer audDClient.Close()

	mux := http.NewServeMux()
	eventBroker := newBroker[Track]()
	stateBroker := newBroker[IdentifierState]()

	mux.HandleFunc("GET /tracks", handleSSE(eventBroker, "track"))
	mux.HandleFunc("GET /state", handleSSE(stateBroker, "state")) // still need to handle state broadcasting

	os.Remove("/tmp/nowplaying.sock")
	socket, err := net.Listen("unix", "/tmp/nowplaying.sock")
	if err != nil {
		log.Fatal("Could not connect to Audio Worker\n", "err: ", err)
	}
	engine := newIdentifyEngine(eventBroker, stateBroker, 30*time.Second)
	go handleIdentify(engine, socket)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		os.Remove("/tmp/nowplaying.sock")
		os.Exit(0)
	}()

	fmt.Println("Server running on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

const idleTimeout = 30 * time.Second

// identifyEngine owns clip processing, idle timing, and track debouncing.
// One engine per process so idle state and current track survive per-clip connections.
type identifyEngine struct {
	trackBroker *SSEBroker[Track]
	stateBroker *SSEBroker[IdentifierState]

	mu          sync.Mutex
	generation  uint64
	currentTrack Track
	identifyMu  sync.Mutex // serialize AudD calls

	idleMu      sync.Mutex
	idleTimer   *time.Timer
	idleTimeout time.Duration
}

func newIdentifyEngine(
	trackBroker *SSEBroker[Track],
	stateBroker *SSEBroker[IdentifierState],
	idleTimeout time.Duration,
) *identifyEngine {
	return &identifyEngine{
		trackBroker: trackBroker,
		stateBroker: stateBroker,
		idleTimeout: idleTimeout,
	}
}

func (e *identifyEngine) resetIdleTimer() {
	e.idleMu.Lock()
	defer e.idleMu.Unlock()
	if e.idleTimer != nil {
		e.idleTimer.Stop()
	}
	e.idleTimer = time.AfterFunc(e.idleTimeout, func() {
		e.stateBroker.publish(StateIdle)
	})
}

func sameTrack(a, b Track) bool {
	return a.Title == b.Title && a.Artist == b.Artist
}

// applyResult publishes track/state updates for a finished identification.
// Returns false when gen is stale and the result was discarded.
func (e *identifyEngine) applyResult(gen uint64, track Track, err error) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if gen != e.generation {
		return false
	}

	if err != nil {
		log.Println("error identifying song:", err)
		e.stateBroker.publish(StateError)
		return true
	}

	if track.Title == "" {
		// AudD no-match: keep whatever is already on screen.
		if e.currentTrack.Title != "" {
			e.stateBroker.publish(StateListening)
		}
		return true
	}

	if sameTrack(track, e.currentTrack) {
		e.stateBroker.publish(StateListening)
		return true
	}

	e.currentTrack = track
	e.trackBroker.publish(track)
	e.stateBroker.publish(StateListening)
	return true
}

func (e *identifyEngine) processClip(audio []byte) {
	e.resetIdleTimer()

	e.mu.Lock()
	e.generation++
	gen := e.generation
	e.mu.Unlock()

	e.stateBroker.publish(StateIdentifying)

	e.identifyMu.Lock()
	defer e.identifyMu.Unlock()

	e.mu.Lock()
	stale := gen != e.generation
	e.mu.Unlock()
	if stale {
		return
	}

	track, err := identifySong(audio)
	e.applyResult(gen, track, err)
}

func handleIdentify(engine *identifyEngine, socket net.Listener) {
	for {
		conn, err := socket.Accept()
		if err != nil {
			log.Println("socket accept error:", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		go func(conn net.Conn) {
			defer conn.Close()

			reader := bufio.NewReaderSize(conn, 64*1024)
			for {
				audio, err := readClip(reader)
				if err != nil {
					if err != io.EOF {
						log.Println("error reading clip:", err)
					}
					return
				}

				go engine.processClip(audio)
			}
		}(conn)
	}
}

func identifySong(audio []byte) (Track, error) {
	resp, err := audDClient.Recognize(audio, &audd.RecognizeOptions{
		ReturnMetadata: "spotify,musicbrainz",
	})
	if err != nil {
		return Track{}, fmt.Errorf("error getting song info: %w", err)
	}

	if resp == nil {
		fmt.Println("no match")
		return Track{}, nil // maybe change status to idle? TODO: Come back to this
	}

	return buildTrack(resp), nil
}

func readClip(r *bufio.Reader) ([]byte, error) {
	headerLine, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}

	var hdr ClipHeader
	if err := json.Unmarshal(headerLine, &hdr); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadClipHeader, err)
	}
	if hdr.Bytes <= 0 || hdr.Bytes > maxClipBytes {
		return nil, fmt.Errorf("%w: %d", errImplausibleClipSize, hdr.Bytes)
	}

	audio := make([]byte, hdr.Bytes)
	if _, err := io.ReadFull(r, audio); err != nil {
		return nil, err
	}
	return audio, nil
}

type spotifyArtist struct {
	Name string `json:"name"`
}

type spotifyImage struct {
	URL string `json:"url"`
}

// isCompilation reports whether a Spotify album is a various-artists compilation
// (a "Pure... Dance Party"-style release) rather than the song's real album.
func isCompilation(albumType string, artists []spotifyArtist) bool {
	if strings.EqualFold(albumType, "compilation") {
		return true
	}
	for _, a := range artists {
		if strings.EqualFold(a.Name, "Various Artists") {
			return true
		}
	}
	return false
}

func buildTrack(resp *audd.Recognition) Track {
	track := Track{
		Album:         resp.Album,
		Artist:        resp.Artist,
		AlbumCoverURL: resp.ThumbnailURL(),
		ISRC:          resp.ISRC,
		ReleaseDate:   resp.ReleaseDate,
		Title:         resp.Title,
	}

	if resp.Spotify != nil {
		if resp.Spotify.DurationMs > 0 {
			track.DurationS = resp.Spotify.DurationMs / 1000
		}

		// Album name, release date, and cover art live in the nested "album"
		// object, which the SDK leaves in Extras as raw JSON.
		if raw, ok := resp.Spotify.Extras["album"]; ok {
			var album struct {
				Name        string          `json:"name"`
				AlbumType   string          `json:"album_type"`
				ReleaseDate string          `json:"release_date"`
				Artists     []spotifyArtist `json:"artists"`
				Images      []spotifyImage  `json:"images"`
			}
			// Skip various-artists compilations ("Pure... Dance Party" and friends):
			// their name, date, and cover all belong to the compilation, not the
			// song's real album. Singles and regular albums are fine.
			if json.Unmarshal(raw, &album) == nil && !isCompilation(album.AlbumType, album.Artists) {
				if album.Name != "" {
					track.Album = album.Name
				}
				if album.ReleaseDate != "" {
					track.ReleaseDate = album.ReleaseDate
				}
				// Spotify orders album images largest-first.
				if len(album.Images) > 0 && album.Images[0].URL != "" {
					track.AlbumCoverURL = album.Images[0].URL
				}
			}
		}
	}

	// MusicBrainz lists every release this recording appears on. AudD defaults to
	// whichever release it indexed (often a soundtrack or compilation), so prefer
	// the original studio album: primary-type "Album" with no secondary types
	// (Compilation, Soundtrack, etc.), favoring an official pressing.
	if len(resp.MusicBrainz) > 0 {
		if resp.MusicBrainz[0].Length > 0 {
			track.DurationS = resp.MusicBrainz[0].Length / 1000 // milliseconds
		}

		if raw, ok := resp.MusicBrainz[0].Extras["releases"]; ok {
			var releases []struct {
				Date         string `json:"date"`
				Status       string `json:"status"`
				ReleaseGroup struct {
					ID             string   `json:"id"`
					Title          string   `json:"title"`
					PrimaryType    string   `json:"primary-type"`
					SecondaryTypes []string `json:"secondary-types"`
				} `json:"release-group"`
			}
			if json.Unmarshal(raw, &releases) == nil {
				best := -1
				for i := range releases {
					rg := releases[i].ReleaseGroup
					if rg.PrimaryType != "Album" || len(rg.SecondaryTypes) > 0 {
						continue // skip singles, compilations, soundtracks
					}
					if best == -1 {
						best = i // first studio album is the fallback
					}
					if releases[i].Status == "Official" {
						best = i // prefer an official pressing
						break
					}
				}
				if best != -1 {
					rg := releases[best].ReleaseGroup
					track.Album = rg.Title
					if releases[best].Date != "" {
						track.ReleaseDate = releases[best].Date
					}
					track.AlbumCoverURL = "https://coverartarchive.org/release-group/" + rg.ID + "/front"
				}
			}
		}
	}

	return track
}

type SSEBroker[E Track | IdentifierState] struct {
	clients map[chan E]bool
	mu      sync.RWMutex
}

func newBroker[E Track | IdentifierState]() *SSEBroker[E] {
	return &SSEBroker[E]{
		clients: make(map[chan E]bool),
	}
}

func (b *SSEBroker[E]) Subscribe() chan E {
	ch := make(chan E, 5) // drop down to 1
	b.mu.Lock()
	b.clients[ch] = true
	b.mu.Unlock()
	return ch
}

func (b *SSEBroker[E]) Unsubscribe(ch chan E) {
	b.mu.Lock()
	delete(b.clients, ch)
	close(ch)
	b.mu.Unlock()
}

func (b *SSEBroker[E]) publish(t E) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.clients {
		select {
		case ch <- t:
		default:
		}
	}
}

func handleSSE[E Track | IdentifierState](broker *SSEBroker[E], eventName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		ch := broker.Subscribe()
		defer broker.Unsubscribe(ch)

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case event := <-ch:
				j, err := json.Marshal(event)

				if err != nil {
					http.Error(
						w,
						err.Error(),
						http.StatusInternalServerError,
					)
					return
				}

				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, j)
				flusher.Flush()
			case <-ticker.C:
				fmt.Fprintf(w, ": keep-alive\n\n")
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}
