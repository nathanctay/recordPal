package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
)

type FingerprintData struct {
	payload     string
	duration_ms int
	rms_energy  float32
}

type Track struct {
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Duration_s int    `json:"duration_s"`
	// Progress int POTENTIALLY HAVE IT RETURN WHERE IN THE SONG THEY ARE TO DISPLAY SONG PROGRESS
	Album           string `json:"album"`
	Album_cover_url string `json:"album_cover_url"`
	// Trivia []string
}

var trackChannel = make(chan Track, 1)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /identify", handleIdentify)
	mux.HandleFunc("GET /events", handleSSE)

	fmt.Println("Server running on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func handleIdentify(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(r.Body)
	if err != nil || len(b) < 1 {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}

	var req FingerprintData
	err = json.Unmarshal(b, &req)
	if err != nil || len(b) < 1 {
		http.Error(w, "error parsing request JSON", http.StatusBadRequest)
		return
	}

	// TODO: identify song

	sampleSong := Track{
		Title:           "Never Gonna Give you up",
		Artist:          "Rick Astley",
		Duration_s:      rand.IntN(500),
		Album:           "Off the wall",
		Album_cover_url: "https://upload.wikimedia.org/wikipedia/en/1/1c/Rick_Astley_-_Whenever_You_Need_Somebody.png",
	}

	// fmt.Println(sampleSong)

	// broadcast the value to the frontend. returning it to the rust layer making the call will do us no good
	trackChannel <- sampleSong
}

// type SSEBroker struct {
// 	clients map[chan Track]struct{}
// 	mu      sync.Mutex
// }

// func (b *SSEBroker) publish(t Track) {
// 	b.mu.Lock()
// 	defer b.mu.Unlock()
// 	for ch := range b.clients {
// 		ch <- t
// 	}
// }

func handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

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
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			return
		}
	}
}
