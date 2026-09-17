package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// sseEventBuffer puffert Events pro Client. Zusammen mit dem Verwerfen
	// veralteter Events sorgt das dafür, dass ein langsamer Client niemals
	// einen Publisher oder andere Clients blockiert.
	sseEventBuffer = 8

	// sseStateEvent ist der Name des Events mit dem vollständigen State.
	sseStateEvent = "state"

	// sseRetryHint teilt dem Client mit, nach welcher Zeit er sich nach einem
	// Verbindungsabbruch erneut verbinden soll.
	sseRetryHint = 5 * time.Second
)

type sseEvent struct {
	name string
	data []byte
}

type sseSubscriber struct {
	events chan sseEvent
}

// sseBroker verteilt Events an beliebig viele parallele Clients. Er kennt
// weder die Matrix noch den Cache und löst deshalb niemals eine Matrix-Abfrage
// aus.
type sseBroker struct {
	mu          sync.Mutex
	subscribers map[*sseSubscriber]struct{}
	closed      bool
}

func newSSEBroker() *sseBroker {
	return &sseBroker{
		subscribers: make(map[*sseSubscriber]struct{}),
	}
}

// Subscribe meldet einen neuen Client an. Nach Close liefert der Broker einen
// bereits geschlossenen Kanal, damit sich der Handler sofort beendet.
func (b *sseBroker) Subscribe() *sseSubscriber {
	subscriber := &sseSubscriber{events: make(chan sseEvent, sseEventBuffer)}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		close(subscriber.events)
		return subscriber
	}

	b.subscribers[subscriber] = struct{}{}
	debugf("SSE client subscribed (%d active)", len(b.subscribers))

	return subscriber
}

func (b *sseBroker) Unsubscribe(subscriber *sseSubscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.subscribers[subscriber]; !ok {
		return
	}

	delete(b.subscribers, subscriber)
	close(subscriber.events)

	debugf("SSE client unsubscribed (%d active)", len(b.subscribers))
}

// Publish stellt das Event allen Clients zu, ohne dabei zu blockieren.
func (b *sseBroker) Publish(event sseEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for subscriber := range b.subscribers {
		select {
		case subscriber.events <- event:
		default:
			// Der Client kommt nicht hinterher. Jedes state-Event enthält
			// den vollständigen State, deshalb darf das älteste Event
			// gefahrlos verworfen werden.
			select {
			case <-subscriber.events:
			default:
			}

			select {
			case subscriber.events <- event:
			default:
			}

			debugf("Dropped buffered SSE event for slow client")
		}
	}
}

func (b *sseBroker) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.subscribers)
}

// Close beendet alle offenen Streams. Das wird beim Shutdown benötigt, damit
// http.Server.Shutdown nicht auf dauerhaft offene SSE-Verbindungen wartet.
func (b *sseBroker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}

	b.closed = true

	for subscriber := range b.subscribers {
		delete(b.subscribers, subscriber)
		close(subscriber.events)
	}
}

// stateNotifier verbindet den Statuscache mit dem Broker. Er fragt die Matrix
// nie selbst ab, sondern veröffentlicht ausschließlich den bereits gecachten
// State - und zwar nur dann, wenn er sich wirklich geändert hat.
type stateNotifier struct {
	matrix *matrixConnection
	broker *sseBroker

	mu        sync.Mutex
	published bool
	revision  uint64
}

func newStateNotifier(matrix *matrixConnection) *stateNotifier {
	return &stateNotifier{
		matrix: matrix,
		broker: newSSEBroker(),
	}
}

func (n *stateNotifier) Subscribe() *sseSubscriber {
	return n.broker.Subscribe()
}

func (n *stateNotifier) Unsubscribe(subscriber *sseSubscriber) {
	n.broker.Unsubscribe(subscriber)
}

func (n *stateNotifier) Close() {
	n.broker.Close()
}

// CurrentEvent liefert den aktuell gecachten State als Event. Existiert noch
// kein gültiger Cache, wird bewusst keine Matrix-Abfrage ausgelöst.
func (n *stateNotifier) CurrentEvent() (sseEvent, bool) {
	fields, _, ok := n.matrix.CachedState()
	if !ok {
		return sseEvent{}, false
	}

	payload, err := json.Marshal(fields)
	if err != nil {
		log.Printf("ERROR encoding matrix state for SSE: %v", err)
		return sseEvent{}, false
	}

	return sseEvent{name: sseStateEvent, data: payload}, true
}

// Publish veröffentlicht den vollständigen State, sofern sich der Cache seit
// der letzten Veröffentlichung geändert hat.
func (n *stateNotifier) Publish() {
	n.mu.Lock()
	defer n.mu.Unlock()

	fields, revision, ok := n.matrix.CachedState()
	if !ok {
		return
	}

	if n.published && n.revision == revision {
		return
	}

	payload, err := json.Marshal(fields)
	if err != nil {
		log.Printf("ERROR encoding matrix state for SSE: %v", err)
		return
	}

	n.published = true
	n.revision = revision

	debugf("Publishing matrix state to SSE clients: %s", payload)
	n.broker.Publish(sseEvent{name: sseStateEvent, data: payload})
}

// writeSSEEvent schreibt ein Event im Server-Sent-Events-Format.
func writeSSEEvent(w io.Writer, event sseEvent) error {
	var builder strings.Builder

	if event.name != "" {
		builder.WriteString("event: ")
		builder.WriteString(event.name)
		builder.WriteString("\n")
	}

	for _, line := range strings.Split(string(event.data), "\n") {
		builder.WriteString("data: ")
		builder.WriteString(line)
		builder.WriteString("\n")
	}

	builder.WriteString("\n")

	_, err := io.WriteString(w, builder.String())
	return err
}

// eventsHandler liefert den vollständigen Controller-State als Server-Sent
// Events. Der Stream bedient sich ausschließlich aus dem Cache; egal wie viele
// Clients verbunden sind, es entstehen dadurch keine zusätzlichen
// Matrix-Abfragen.
func (a *apiServer) eventsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   "streaming_unsupported",
			"message": "HTTP server does not support streaming responses",
		})
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")

	// Erst abonnieren, dann den Cache lesen. So kann zwischen initialem State
	// und Stream kein Update verloren gehen.
	subscriber := a.events.Subscribe()
	defer a.events.Unsubscribe(subscriber)

	w.WriteHeader(http.StatusOK)

	if _, err := fmt.Fprintf(w, "retry: %d\n\n", sseRetryHint.Milliseconds()); err != nil {
		return
	}
	flusher.Flush()

	// Existiert noch kein gültiger Cache, wartet der Client auf das nächste
	// reguläre Statusupdate. Eine Matrix-Abfrage wird dafür nicht ausgelöst.
	if event, ok := a.events.CurrentEvent(); ok {
		if err := writeSSEEvent(w, event); err != nil {
			return
		}
		flusher.Flush()
	}

	keepalive := time.NewTicker(a.keepaliveInterval)
	defer keepalive.Stop()

	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			debugf("SSE client disconnected: %v", ctx.Err())
			return
		case event, ok := <-subscriber.events:
			if !ok {
				// Broker geschlossen, der Controller fährt herunter.
				return
			}

			if err := writeSSEEvent(w, event); err != nil {
				debugf("Stopping SSE stream: %v", err)
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				debugf("Stopping SSE stream: %v", err)
				return
			}
			flusher.Flush()
		}
	}
}
