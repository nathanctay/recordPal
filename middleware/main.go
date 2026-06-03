package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
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

type IdentifierState string

const (
	StateIdle        IdentifierState = "idle"
	StateListening   IdentifierState = "listening"
	StateIdentifying IdentifierState = "identifying"
	StateError       IdentifierState = "error"
)

func main() {
	mux := http.NewServeMux()
	eventBroker := newBroker[Track]()
	stateBroker := newBroker[IdentifierState]()

	mux.HandleFunc("GET /tracks", handleSSE(eventBroker, "track"))
	mux.HandleFunc("GET /state", handleSSE(stateBroker, "state")) // still need to handle state broadcasting

	os.Remove("/tmp/shazam.sock")
	socket, err := net.Listen("unix", "/tmp/shazam.sock")
	if err != nil {
		log.Fatal("Could not connect to Audio Worker\n", "err: ", err)
	}
	go handleIdentify(eventBroker, stateBroker, socket)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		os.Remove("/tmp/shazam.sock")
		os.Exit(1)
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

			idleTimer := time.AfterFunc(30*time.Second, func() {
				stateBroker.publish(StateIdle)
			})
			for scanner.Scan() {

				idleTimer.Reset(30 * time.Second)

				var req FingerprintData
				err := json.Unmarshal(scanner.Bytes(), &req)
				stateBroker.publish(StateIdentifying) // find the best place to do this

				if err != nil {
					log.Println("error parsing request JSON:", err)
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
				trackBroker.publish(sampleSong)
				stateBroker.publish(StateListening)
			}
		}(conn)

	}
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
