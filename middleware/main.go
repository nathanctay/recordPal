package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

type FingerprintData struct {
	Payload   string  `json:"payload"`
	DurationS int     `json:"duration_s"`
	RmsEnergy float32 `json:"rms_energy"`
}

type Track struct {
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	DurationS int    `json:"duration_s"`
	// Progress int POTENTIALLY HAVE IT RETURN WHERE IN THE SONG THEY ARE TO DISPLAY SONG PROGRESS
	Album         string `json:"album"`
	AlbumCoverURL string `json:"album_cover_url"`
	// Trivia []string
}

type Artist struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ReleaseGroup struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Type           string   `json:"type"`
	SecondaryTypes []string `json:"secondarytypes"`
	Artists        []Artist `json:"artists"`
}

type Recording struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Duration      float64        `json:"duration"`
	Artists       []Artist       `json:"artists"`
	ReleaseGroups []ReleaseGroup `json:"releasegroups"`
}

type AcoustIDResponse struct {
	Status  string `json:"status"`
	Results []struct {
		ID         string      `json:"id"`
		Score      float64     `json:"score"`
		Recordings []Recording `json:"recordings"`
	} `json:"results"`
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

func main() {
	if _, ok := os.LookupEnv("ACOUSTID_API_KEY"); !ok {
		log.Fatal("ACOUSTID_API_KEY is not set")
	}

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

				var req FingerprintData
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

var acoustidClient = &http.Client{Timeout: 10 * time.Second}

func identifySong(fingerprint FingerprintData) (Track, error) {
	acoustidApiKey, exists := os.LookupEnv("ACOUSTID_API_KEY")

	if !exists {
		return Track{}, fmt.Errorf("Acoustid Api key is not set")
	}

	params := url.Values{}
	params.Set("client", acoustidApiKey)
	params.Set("duration", strconv.Itoa(fingerprint.DurationS))
	params.Set("fingerprint", fingerprint.Payload)
	params.Set("meta", "recordings releasegroups")

	endpoint := "https://api.acoustid.org/v2/lookup?" + params.Encode()

	resp, err := acoustidClient.Get(endpoint)
	if err != nil {
		return Track{}, fmt.Errorf("error getting song info: %w", err)
	}
	defer resp.Body.Close()

	var result AcoustIDResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return Track{}, fmt.Errorf("error decoding song info: %w", err)
	}

	if result.Status != "ok" || len(result.Results) == 0 {
		return Track{}, fmt.Errorf("no match found")
	}

	// Pick the result with the highest score that actually has a recording.
	best := -1
	for i, r := range result.Results {
		if len(r.Recordings) == 0 {
			continue
		}
		if best == -1 || r.Score > result.Results[best].Score {
			best = i
		}
	}

	if best == -1 {
		return Track{}, fmt.Errorf("no match found")
	}

	rec := result.Results[best].Recordings[0]

	track := Track{
		Title:     rec.Title,
		DurationS: int(rec.Duration),
	}

	if len(rec.Artists) > 0 {
		track.Artist = rec.Artists[0].Name
	}

	// Prefer a primary studio album, otherwise fall back to the first release group.
	if len(rec.ReleaseGroups) > 0 {
		chosen := rec.ReleaseGroups[0]
		for _, rg := range rec.ReleaseGroups {
			if rg.Type == "Album" && len(rg.SecondaryTypes) == 0 {
				chosen = rg
				break
			}
		}
		track.Album = chosen.Title
		// AcoustID doesn't return cover art, but the release-group MBID maps to one in the Cover Art Archive.
		track.AlbumCoverURL = fmt.Sprintf("https://coverartarchive.org/release-group/%s/front", chosen.ID)
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
