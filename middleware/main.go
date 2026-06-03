package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

type FingerprintData struct {
	Payload    string  `json:"payload"`
	DurationMs int     `json:"duration_ms"`
	RmsEnergy  float32 `json:"rms_energy"`
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

func main() {
	mux := http.NewServeMux()
	broker := newBroker()

	mux.HandleFunc("POST /identify", handleIdentify(broker))
	mux.HandleFunc("GET /events", handleSSE(broker))

	fmt.Println("Server running on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func handleIdentify(broker *SSEBroker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil || len(b) < 1 {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}

		var req FingerprintData
		err = json.Unmarshal(b, &req)
		if err != nil {
			http.Error(w, "error parsing request JSON", http.StatusBadRequest)
			return
		}

		// TODO: identify song

		sampleSong := Track{
			Title:         "Never Gonna Give you up",
			Artist:        "Rick Astley",
			DurationS:     rand.IntN(500), // this is just for a tiny bit of variety in the samples
			Album:         "Off the wall",
			AlbumCoverURL: "https://upload.wikimedia.org/wikipedia/en/1/1c/Rick_Astley_-_Whenever_You_Need_Somebody.png",
		}

		// broadcast the value to the frontend. returning it to the rust layer making the call will do us no good
		broker.publish(sampleSong)
		w.WriteHeader(http.StatusOK)
	}
}

type SSEBroker struct {
	clients map[chan Track]bool
	mu      sync.RWMutex
}

func newBroker() *SSEBroker {
	return &SSEBroker{
		clients: make(map[chan Track]bool),
	}
}

func (b *SSEBroker) Subscribe() chan Track {
	ch := make(chan Track, 5) // drop down to 1
	b.mu.Lock()
	b.clients[ch] = true
	b.mu.Unlock()
	return ch
}

func (b *SSEBroker) Unsubscribe(ch chan Track) {
	b.mu.Lock()
	delete(b.clients, ch)
	close(ch)
	b.mu.Unlock()
}

func (b *SSEBroker) publish(t Track) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.clients {
		select {
		case ch <- t:
		default:
		}
	}
}

func handleSSE(broker *SSEBroker) http.HandlerFunc {
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
		trackChannel := broker.Subscribe()
		defer broker.Unsubscribe(trackChannel)

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case track := <-trackChannel:
				j, err := json.Marshal(track)

				if err != nil {
					http.Error(
						w,
						err.Error(),
						http.StatusInternalServerError,
					)
					return
				}

				fmt.Fprintf(w, "data: %s\n\n", j)
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
