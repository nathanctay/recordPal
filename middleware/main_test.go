package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	audd "github.com/AudDMusic/audd-go"
)

func writeFramedClip(w io.Writer, audio []byte) error {
	hdr, err := json.Marshal(ClipHeader{
		Type:   "clip",
		Format: "wav",
		Bytes:  len(audio),
	})
	if err != nil {
		return err
	}
	if _, err := w.Write(append(hdr, '\n')); err != nil {
		return err
	}
	_, err = w.Write(audio)
	return err
}

// AudD response → Track mapping.
func TestBuildTrack(t *testing.T) {
	tests := []struct {
		name string
		resp *audd.Recognition
		want Track
	}{
		{
			name: "top-level fields only",
			resp: &audd.Recognition{
				Artist:      "Imagine Dragons",
				Title:       "Warriors",
				Album:       "Hits 2014",
				ISRC:        "USUM71414163",
				ReleaseDate: "2020-03-13",
				SongLink:    "https://lis.tn/Warriors",
			},
			want: Track{
				Album:         "Hits 2014",
				AlbumCoverURL: "https://lis.tn/Warriors?thumb",
				Artist:        "Imagine Dragons",
				ISRC:          "USUM71414163",
				ReleaseDate:   "2020-03-13",
				Title:         "Warriors",
			},
		},
		{
			name: "spotify overrides album, date, cover, and duration",
			resp: &audd.Recognition{
				Artist: "Imagine Dragons",
				Title:  "Warriors",
				Album:  "Hits 2014",
				Spotify: &audd.SpotifyMetadata{
					DurationMs: 170000,
					Extras: map[string]json.RawMessage{
						"album": json.RawMessage(`{
							"name": "Smoke + Mirrors",
							"album_type": "album",
							"release_date": "2015-02-17",
							"images": [
								{"url": "https://i.scdn.co/large"},
								{"url": "https://i.scdn.co/small"}
							]
						}`),
					},
				},
			},
			want: Track{
				Album:         "Smoke + Mirrors",
				AlbumCoverURL: "https://i.scdn.co/large",
				Artist:        "Imagine Dragons",
				DurationS:     170,
				ReleaseDate:   "2015-02-17",
				Title:         "Warriors",
			},
		},
		{
			name: "spotify compilation is skipped, falls back to audd album",
			resp: &audd.Recognition{
				Artist:      "Imagine Dragons",
				Title:       "Warriors",
				Album:       "Warriors",
				ReleaseDate: "2014-09-18",
				SongLink:    "https://lis.tn/Warriors",
				Spotify: &audd.SpotifyMetadata{
					DurationMs: 170000,
					Extras: map[string]json.RawMessage{
						"album": json.RawMessage(`{
							"name": "Pure... Dance Party",
							"album_type": "compilation",
							"release_date": "2019-01-01",
							"images": [{"url": "https://i.scdn.co/comp"}]
						}`),
					},
				},
			},
			want: Track{
				Album:         "Warriors",
				AlbumCoverURL: "https://lis.tn/Warriors?thumb",
				Artist:        "Imagine Dragons",
				DurationS:     170,
				ReleaseDate:   "2014-09-18",
				Title:         "Warriors",
			},
		},
		{
			name: "spotify various-artists album is skipped",
			resp: &audd.Recognition{
				Artist:      "Imagine Dragons",
				Title:       "Warriors",
				Album:       "Warriors",
				ReleaseDate: "2014-09-18",
				SongLink:    "https://lis.tn/Warriors",
				Spotify: &audd.SpotifyMetadata{
					DurationMs: 170000,
					Extras: map[string]json.RawMessage{
						"album": json.RawMessage(`{
							"name": "Massive Dance Hits",
							"album_type": "album",
							"release_date": "2019-01-01",
							"artists": [{"name": "Various Artists"}],
							"images": [{"url": "https://i.scdn.co/comp"}]
						}`),
					},
				},
			},
			want: Track{
				Album:         "Warriors",
				AlbumCoverURL: "https://lis.tn/Warriors?thumb",
				Artist:        "Imagine Dragons",
				DurationS:     170,
				ReleaseDate:   "2014-09-18",
				Title:         "Warriors",
			},
		},
		{
			name: "musicbrainz prefers official studio album over compilation",
			resp: &audd.Recognition{
				Artist: "Imagine Dragons",
				Title:  "Warriors",
				Album:  "Hits 2014",
				MusicBrainz: []audd.MusicBrainzEntry{{
					Length: 170066,
					Extras: map[string]json.RawMessage{
						"releases": json.RawMessage(`[
							{"date":"2014-11-01","status":"Official","release-group":{"id":"comp-id","title":"Now That's What I Call Music","primary-type":"Album","secondary-types":["Compilation"]}},
							{"date":"2015-02-17","status":"Official","release-group":{"id":"a186ae54","title":"Smoke + Mirrors","primary-type":"Album","secondary-types":null}}
						]`),
					},
				}},
			},
			want: Track{
				Album:         "Smoke + Mirrors",
				AlbumCoverURL: "https://coverartarchive.org/release-group/a186ae54/front",
				Artist:        "Imagine Dragons",
				DurationS:     170,
				ReleaseDate:   "2015-02-17",
				Title:         "Warriors",
			},
		},
		{
			name: "musicbrainz with no studio album keeps existing album",
			resp: &audd.Recognition{
				Artist: "Imagine Dragons",
				Title:  "Warriors",
				Album:  "Hits 2014",
				MusicBrainz: []audd.MusicBrainzEntry{{
					Length: 170000,
					Extras: map[string]json.RawMessage{
						"releases": json.RawMessage(`[
							{"date":"2014-11-01","status":"Official","release-group":{"id":"comp-id","title":"Monsoon Love Vol 19","primary-type":"Album","secondary-types":["Compilation"]}}
						]`),
					},
				}},
			},
			want: Track{
				Album:     "Hits 2014",
				Artist:    "Imagine Dragons",
				DurationS: 170,
				Title:     "Warriors",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildTrack(tt.resp)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildTrack() mismatch\n got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

// JSON header line + raw WAV bytes from the unix socket.
func TestReadClip(t *testing.T) {
	validAudio := []byte("RIFF....WAVEfmt ")

	tests := []struct {
		name    string
		input   string
		want    []byte
		wantErr error
	}{
		{
			name:  "valid clip",
			input: mustFramedClip(t, validAudio),
			want:  validAudio,
		},
		{
			name:    "bad header json",
			input:   "not json\n",
			wantErr: errBadClipHeader,
		},
		{
			name:    "zero byte payload",
			input:   `{"type":"clip","format":"wav","bytes":0}` + "\n",
			wantErr: errImplausibleClipSize,
		},
		{
			name:    "negative byte count",
			input:   `{"type":"clip","format":"wav","bytes":-1}` + "\n",
			wantErr: errImplausibleClipSize,
		},
		{
			name:    "payload over limit",
			input:   `{"type":"clip","format":"wav","bytes":10485761}` + "\n",
			wantErr: errImplausibleClipSize,
		},
		{
			name:    "truncated payload",
			input:   `{"type":"clip","format":"wav","bytes":4}` + "\n" + "ab",
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			name:    "empty stream",
			input:   "",
			wantErr: io.EOF,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tt.input))
			got, err := readClip(r)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("readClip() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readClip() unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("readClip() = %q, want %q", got, tt.want)
			}
		})
	}
}

func mustFramedClip(t *testing.T, audio []byte) string {
	t.Helper()
	var b strings.Builder
	if err := writeFramedClip(&b, audio); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Subscribers receive published events.
func TestBrokerDeliversToSubscriber(t *testing.T) {
	b := newBroker[IdentifierState]()
	ch := b.Subscribe()
	defer b.Unsubscribe(ch)

	b.publish(StateIdentifying)

	select {
	case got := <-ch:
		if got != StateIdentifying {
			t.Errorf("got %q, want %q", got, StateIdentifying)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for published event")
	}
}

// Unsubscribe closes the channel; publish with no subscribers is safe.
func TestBrokerUnsubscribe(t *testing.T) {
	b := newBroker[IdentifierState]()
	ch := b.Subscribe()
	b.Unsubscribe(ch)

	if _, ok := <-ch; ok {
		t.Fatal("expected channel to be closed after Unsubscribe")
	}

	b.publish(StateIdle)
}

// Full buffers drop events instead of blocking publish.
func TestBrokerDropsWhenBufferFull(t *testing.T) {
	b := newBroker[IdentifierState]()
	ch := b.Subscribe()
	defer b.Unsubscribe(ch)

	for i := 0; i < 7; i++ {
		b.publish(StateListening)
	}

	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			if count != 5 {
				t.Errorf("buffered %d events, want 5", count)
			}
			return
		}
	}
}

// Broker publish → SSE event frame on the wire.
func TestHandleSSEDeliverEvent(t *testing.T) {
	broker := newBroker[IdentifierState]()
	handler := handleSSE(broker, "state")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/state", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler(rec, req)
		close(done)
	}()

	publishUntil(t, broker, rec, StateIdentifying, `"identifying"`)

	body := rec.Body.String()
	if !strings.Contains(body, "event: state\n") {
		t.Errorf("missing event line in body:\n%s", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not exit after context cancel")
	}
}

// Handler exits when the client disconnects.
func TestHandleSSEContextCancel(t *testing.T) {
	broker := newBroker[Track]()
	handler := handleSSE(broker, "track")

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/tracks", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not exit after context cancel")
	}
}

func publishUntil[E Track | IdentifierState](t *testing.T, broker *SSEBroker[E], rec *httptest.ResponseRecorder, event E, wantInBody string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		broker.publish(event)
		if strings.Contains(rec.Body.String(), wantInBody) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("body never contained %q:\n%s", wantInBody, rec.Body.String())
}

func TestSameTrack(t *testing.T) {
	a := Track{Title: "Warriors", Artist: "Imagine Dragons"}
	b := Track{Title: "Warriors", Artist: "Imagine Dragons"}
	c := Track{Title: "Believer", Artist: "Imagine Dragons"}

	if !sameTrack(a, b) {
		t.Fatal("expected same track")
	}
	if sameTrack(a, c) {
		t.Fatal("expected different tracks")
	}
}

func TestApplyResultStaleGeneration(t *testing.T) {
	trackBroker := newBroker[Track]()
	stateBroker := newBroker[IdentifierState]()
	engine := newIdentifyEngine(trackBroker, stateBroker, time.Minute)

	engine.generation = 2
	published := engine.applyResult(1, Track{Title: "Warriors", Artist: "Imagine Dragons"}, nil)
	if published {
		t.Fatal("expected stale result to be discarded")
	}
}

func TestApplyResultNoMatchKeepsCurrentTrack(t *testing.T) {
	trackBroker := newBroker[Track]()
	stateBroker := newBroker[IdentifierState]()
	engine := newIdentifyEngine(trackBroker, stateBroker, time.Minute)
	engine.generation = 1
	engine.currentTrack = Track{Title: "Warriors", Artist: "Imagine Dragons"}

	stateCh := stateBroker.Subscribe()
	defer stateBroker.Unsubscribe(stateCh)

	if !engine.applyResult(1, Track{}, nil) {
		t.Fatal("expected no-match result to be handled")
	}

	select {
	case state := <-stateCh:
		if state != StateListening {
			t.Fatalf("state = %q, want %q", state, StateListening)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for listening state")
	}
}

func TestApplyResultDebouncesSameTrack(t *testing.T) {
	trackBroker := newBroker[Track]()
	stateBroker := newBroker[IdentifierState]()
	engine := newIdentifyEngine(trackBroker, stateBroker, time.Minute)
	engine.generation = 1
	engine.currentTrack = Track{Title: "Warriors", Artist: "Imagine Dragons"}

	trackCh := trackBroker.Subscribe()
	defer trackBroker.Unsubscribe(trackCh)

	if !engine.applyResult(1, Track{Title: "Warriors", Artist: "Imagine Dragons"}, nil) {
		t.Fatal("expected same-track result to be handled")
	}

	select {
	case <-trackCh:
		t.Fatal("did not expect a duplicate track publish")
	default:
	}
}

func TestApplyResultPublishesNewTrack(t *testing.T) {
	trackBroker := newBroker[Track]()
	stateBroker := newBroker[IdentifierState]()
	engine := newIdentifyEngine(trackBroker, stateBroker, time.Minute)
	engine.generation = 1

	trackCh := trackBroker.Subscribe()
	defer trackBroker.Unsubscribe(trackCh)

	want := Track{Title: "Warriors", Artist: "Imagine Dragons"}
	if !engine.applyResult(1, want, nil) {
		t.Fatal("expected new track to be published")
	}

	select {
	case got := <-trackCh:
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("track = %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for track publish")
	}
}
