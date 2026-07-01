package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	audd "github.com/AudDMusic/audd-go"
	"github.com/joho/godotenv"
)

type RecordingData struct {
	Payload   string  `json:"payload"`
	DurationS int     `json:"duration_s"`
	RmsEnergy float32 `json:"rms_energy"`
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

// type MusicBrainzEntry struct {
// 	Length int                        `json:"length,omitempty"`
// 	Extras map[string]json.RawMessage `json:"-"`
// }

// type AudDResponse struct {
// 	Status string `json:"status"`
// 	Result []struct {
// 		Artist      string             `json:"artist"`
// 		Title       string             `json:"title"`
// 		Album       string             `json:"album"`
// 		ReleaseDate string             `json:"release_date"`
// 		MusicBrainz []MusicBrainzEntry `json:"musicbrainz,omitempty"`
// 	} `json:"result"`
// }

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

	audDClient = audd.NewClient(audDApiKey)
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
	go handleIdentify(eventBroker, stateBroker, socket)

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

func handleIdentify(trackBroker *SSEBroker[Track], stateBroker *SSEBroker[IdentifierState], socket net.Listener) {
	for {
		conn, err := socket.Accept()
		if err != nil {
			log.Println("socket accept error:", err)
			return
		}

		go func(conn net.Conn) {
			defer conn.Close()

			scanner := bufio.NewScanner(conn)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // up to 1MB lines

			const idleTimeout = 30 * time.Second
			// switch our listening status to idle after 30 seconds
			idleTimer := time.AfterFunc(idleTimeout, func() {
				stateBroker.publish(StateIdle)
			})
			defer idleTimer.Stop()

			for scanner.Scan() {

				idleTimer.Reset(idleTimeout)

				var req RecordingData
				err := json.Unmarshal(scanner.Bytes(), &req)

				if err != nil {
					log.Println("error parsing request JSON:", err)
					continue
				}

				stateBroker.publish(StateIdentifying) // find the best place to do this

				//identify the song
				track, err := identifySong(req)
				if err != nil {
					log.Println("error identifying song:", err)
					stateBroker.publish(StateError)
					continue
				}

				// broadcast the value to the frontend
				trackBroker.publish(track)
				stateBroker.publish(StateListening)
			}

			if err := scanner.Err(); err != nil {
				log.Println("scanner error:", err)
			}
		}(conn)

	}
}

func identifySong(recording RecordingData) (Track, error) {
	resp, err := audDClient.Recognize("PLACEHOLDER TEXT", &audd.RecognizeOptions{ //TODO: Replace placeholder
		ReturnMetadata: "spotify,musicbrainz",
	})
	if err != nil {
		return Track{}, fmt.Errorf("error getting song info: %w", err)
	}

	if resp == nil {
		fmt.Println("no match")
		return Track{}, nil // maybe change status to idle? TODO: Come back to this
	}

	fmt.Println(string(resp.RawResponse))
	b, _ := json.MarshalIndent(resp, "", "  ")
	fmt.Println(string(b))

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
				Name        string `json:"name"`
				AlbumType   string `json:"album_type"`
				ReleaseDate string `json:"release_date"`
				Images      []struct {
					URL string `json:"url"`
				} `json:"images"`
			}
			if json.Unmarshal(raw, &album) == nil {
				fmt.Printf("spotify album: %q (type: %s)\n", album.Name, album.AlbumType)
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

	return track, nil
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
