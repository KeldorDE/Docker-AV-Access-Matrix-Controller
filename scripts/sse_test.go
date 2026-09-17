package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testMatrixTimeout = 750 * time.Millisecond
	testEventTimeout  = 3 * time.Second
	testQuietPeriod   = 250 * time.Millisecond
)

type testController struct {
	fake   *fakeMatrix
	matrix *matrixConnection
	events *stateNotifier
	server *httptest.Server
}

// newTestController baut den kompletten Controller gegen die Fake-Matrix auf.
func newTestController(t *testing.T, keepalive time.Duration) *testController {
	t.Helper()

	fake := startFakeMatrix(t)
	matrix := newTestMatrix(t, fake)

	if err := matrix.Connect(); err != nil {
		t.Fatalf("connecting to fake matrix: %v", err)
	}
	if err := matrix.RefreshStatus(); err != nil {
		t.Fatalf("initial status refresh: %v", err)
	}

	events := newStateNotifier(matrix)
	events.Publish()

	server := httptest.NewServer(newRouter(newAPIServer(matrix, events, keepalive)))

	t.Cleanup(func() {
		events.Close()
		server.Close()
	})

	return &testController{
		fake:   fake,
		matrix: matrix,
		events: events,
		server: server,
	}
}

type sseFrame struct {
	name    string
	data    string
	comment string
}

type sseTestClient struct {
	frames chan sseFrame
	cancel context.CancelFunc
	body   func()
	header http.Header
	status int
}

func (c *testController) connectSSE(t *testing.T) *sseTestClient {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.server.URL+"/events", nil)
	if err != nil {
		cancel()
		t.Fatalf("building /events request: %v", err)
	}

	response, err := c.server.Client().Do(request)
	if err != nil {
		cancel()
		t.Fatalf("connecting to /events: %v", err)
	}

	client := &sseTestClient{
		frames: make(chan sseFrame, 64),
		cancel: cancel,
		body:   func() { _ = response.Body.Close() },
		header: response.Header,
		status: response.StatusCode,
	}

	go func() {
		reader := bufio.NewReader(response.Body)
		frame := sseFrame{}

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(client.frames)
				return
			}

			line = strings.TrimRight(line, "\r\n")

			switch {
			case line == "":
				if frame.name != "" || frame.data != "" {
					client.frames <- frame
				}
				frame = sseFrame{}
			case strings.HasPrefix(line, ":"):
				client.frames <- sseFrame{comment: strings.TrimSpace(line[1:])}
			case strings.HasPrefix(line, "event:"):
				frame.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				if frame.data != "" {
					frame.data += "\n"
				}
				frame.data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}()

	t.Cleanup(client.Close)

	return client
}

func (c *sseTestClient) Close() {
	c.cancel()
	c.body()
}

// nextState wartet auf das nächste state-Event und ignoriert Keepalives.
func (c *sseTestClient) nextState(t *testing.T) map[string]any {
	t.Helper()

	deadline := time.After(testEventTimeout)

	for {
		select {
		case frame, ok := <-c.frames:
			if !ok {
				t.Fatalf("SSE stream closed while waiting for a state event")
			}
			if frame.name != sseStateEvent {
				continue
			}

			var state map[string]any
			if err := json.Unmarshal([]byte(frame.data), &state); err != nil {
				t.Fatalf("decoding state event %q: %v", frame.data, err)
			}
			return state
		case <-deadline:
			t.Fatalf("timed out waiting for a state event")
		}
	}
}

func (c *sseTestClient) nextFrame(t *testing.T, timeout time.Duration) sseFrame {
	t.Helper()

	select {
	case frame, ok := <-c.frames:
		if !ok {
			t.Fatalf("SSE stream closed while waiting for a frame")
		}
		return frame
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for an SSE frame")
	}

	return sseFrame{}
}

// expectNoState stellt sicher, dass in der Wartezeit kein state-Event kommt.
func (c *sseTestClient) expectNoState(t *testing.T, wait time.Duration) {
	t.Helper()

	deadline := time.After(wait)

	for {
		select {
		case frame, ok := <-c.frames:
			if !ok {
				return
			}
			if frame.name == sseStateEvent {
				t.Fatalf("received unexpected state event: %s", frame.data)
			}
		case <-deadline:
			return
		}
	}
}

func TestEventsStreamContentType(t *testing.T) {
	controller := newTestController(t, time.Minute)
	client := controller.connectSSE(t)

	if client.status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", client.status)
	}

	if contentType := client.header.Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("expected Content-Type text/event-stream, got %q", contentType)
	}

	if cacheControl := client.header.Get("Cache-Control"); !strings.Contains(cacheControl, "no-cache") {
		t.Fatalf("expected Cache-Control to contain no-cache, got %q", cacheControl)
	}
}

func TestEventsSendsCachedStateOnConnect(t *testing.T) {
	controller := newTestController(t, time.Minute)
	client := controller.connectSSE(t)

	state := client.nextState(t)

	if got := state["out1_in"]; got != float64(1) {
		t.Fatalf("expected out1_in=1 in initial state, got %v", got)
	}
	if got := state["edid_in1"]; got != float64(5) {
		t.Fatalf("expected edid_in1=5 in initial state, got %v", got)
	}
}

// Der SSE-Stream muss exakt denselben logischen Zustand liefern wie /status.
func TestEventsStateMatchesStatusEndpoint(t *testing.T) {
	controller := newTestController(t, time.Minute)
	client := controller.connectSSE(t)

	state := client.nextState(t)

	response, err := controller.server.Client().Get(controller.server.URL + "/status")
	if err != nil {
		t.Fatalf("requesting /status: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected /status 200, got %d", response.StatusCode)
	}

	var status map[string]any
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatalf("decoding /status: %v", err)
	}

	if len(status) != len(state) {
		t.Fatalf("/status has %d fields, SSE state has %d", len(status), len(state))
	}

	for key, value := range status {
		if state[key] != value {
			t.Fatalf("field %q differs: /status=%v sse=%v", key, value, state[key])
		}
	}
}

// Ein neuer Client darf keine zusätzliche Matrix-Abfrage auslösen.
func TestEventsConnectDoesNotQueryMatrix(t *testing.T) {
	controller := newTestController(t, time.Minute)
	controller.fake.ResetCommands()

	first := controller.connectSSE(t)
	first.nextState(t)

	second := controller.connectSSE(t)
	second.nextState(t)

	if commands := controller.fake.Commands(); len(commands) != 0 {
		t.Fatalf("expected no matrix commands for SSE clients, got %v", commands)
	}
}

// Ohne gültigen Cache wird kein State geschickt und nichts abgefragt.
func TestEventsWithoutCacheSendsNoState(t *testing.T) {
	fake := startFakeMatrix(t)
	matrix := newTestMatrix(t, fake)
	events := newStateNotifier(matrix)

	server := httptest.NewServer(newRouter(newAPIServer(matrix, events, time.Minute)))
	t.Cleanup(func() {
		events.Close()
		server.Close()
	})

	controller := &testController{fake: fake, matrix: matrix, events: events, server: server}

	client := controller.connectSSE(t)
	client.expectNoState(t, testQuietPeriod)

	if commands := fake.Commands(); len(commands) != 0 {
		t.Fatalf("expected no matrix commands, got %v", commands)
	}
}

// Eine physische Umschaltung wird vom schnellen Poll erkannt und als genau ein
// Event mit dem vollständigen State veröffentlicht.
func TestFastPollPublishesPhysicalChange(t *testing.T) {
	controller := newTestController(t, time.Minute)
	client := controller.connectSSE(t)
	client.nextState(t)

	controller.fake.SwitchPhysically(2, 3)

	if err := controller.matrix.RefreshRouting(); err != nil {
		t.Fatalf("refreshing routing: %v", err)
	}
	controller.events.Publish()

	state := client.nextState(t)
	if got := state["out2_in"]; got != float64(3) {
		t.Fatalf("expected out2_in=3, got %v", got)
	}
	if got := state["edid_in1"]; got != float64(5) {
		t.Fatalf("expected full state including edid_in1=5, got %v", got)
	}

	client.expectNoState(t, testQuietPeriod)
}

// Mehrere gleichzeitig verbundene Clients erhalten dieselbe Änderung.
func TestEventsBroadcastToAllClients(t *testing.T) {
	controller := newTestController(t, time.Minute)

	clients := []*sseTestClient{
		controller.connectSSE(t),
		controller.connectSSE(t),
		controller.connectSSE(t),
	}
	for _, client := range clients {
		client.nextState(t)
	}

	controller.fake.SwitchPhysically(4, 2)
	controller.fake.ResetCommands()

	if err := controller.matrix.RefreshRouting(); err != nil {
		t.Fatalf("refreshing routing: %v", err)
	}
	controller.events.Publish()

	for index, client := range clients {
		state := client.nextState(t)
		if got := state["out4_in"]; got != float64(2) {
			t.Fatalf("client %d expected out4_in=2, got %v", index, got)
		}
	}

	// Ein einziger Poll versorgt alle Clients.
	if commands := controller.fake.Commands(); len(commands) != 1 {
		t.Fatalf("expected exactly one matrix command per poll, got %v", commands)
	}
}

// Liefert ein Poll denselben Zustand, entsteht kein weiteres Event.
func TestUnchangedPollPublishesNoEvent(t *testing.T) {
	controller := newTestController(t, time.Minute)
	client := controller.connectSSE(t)
	client.nextState(t)

	for i := 0; i < 3; i++ {
		if err := controller.matrix.RefreshRouting(); err != nil {
			t.Fatalf("refreshing routing: %v", err)
		}
		controller.events.Publish()
	}

	if err := controller.matrix.RefreshStatus(); err != nil {
		t.Fatalf("refreshing status: %v", err)
	}
	controller.events.Publish()

	client.expectNoState(t, testQuietPeriod)
}

// Ein erfolgreiches REST-Kommando löst sofort ein State-Event aus.
func TestRESTSwitchPublishesState(t *testing.T) {
	controller := newTestController(t, time.Minute)
	client := controller.connectSSE(t)
	client.nextState(t)

	response, err := controller.server.Client().Post(
		controller.server.URL+"/switch",
		"application/json",
		bytes.NewBufferString(`{"input":3,"output":1}`),
	)
	if err != nil {
		t.Fatalf("posting /switch: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected /switch 200, got %d", response.StatusCode)
	}

	state := client.nextState(t)
	if got := state["out1_in"]; got != float64(3) {
		t.Fatalf("expected out1_in=3 after switch, got %v", got)
	}
}

func TestRESTEDIDPublishesState(t *testing.T) {
	controller := newTestController(t, time.Minute)
	client := controller.connectSSE(t)
	client.nextState(t)

	response, err := controller.server.Client().Post(
		controller.server.URL+"/edid",
		"application/json",
		bytes.NewBufferString(`{"input":2,"edid":11}`),
	)
	if err != nil {
		t.Fatalf("posting /edid: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected /edid 200, got %d", response.StatusCode)
	}

	state := client.nextState(t)
	if got := state["edid_in2"]; got != float64(11) {
		t.Fatalf("expected edid_in2=11 after EDID change, got %v", got)
	}
}

// Ein fehlgeschlagenes REST-Kommando darf kein Event erzeugen.
func TestFailedRESTCommandPublishesNoState(t *testing.T) {
	controller := newTestController(t, time.Minute)
	client := controller.connectSSE(t)
	client.nextState(t)

	response, err := controller.server.Client().Post(
		controller.server.URL+"/switch",
		"application/json",
		bytes.NewBufferString(`{"input":99,"output":1}`),
	)
	if err != nil {
		t.Fatalf("posting /switch: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected /switch 400, got %d", response.StatusCode)
	}

	client.expectNoState(t, testQuietPeriod)
}

func TestEventsKeepaliveWithoutStateChange(t *testing.T) {
	controller := newTestController(t, 50*time.Millisecond)
	client := controller.connectSSE(t)
	client.nextState(t)

	frame := client.nextFrame(t, testEventTimeout)
	if frame.comment != "keepalive" {
		t.Fatalf("expected keepalive comment, got %+v", frame)
	}

	if commands := controller.fake.Commands(); len(commands) == 0 {
		t.Fatalf("expected initial commands to be recorded")
	}

	controller.fake.ResetCommands()
	client.nextFrame(t, testEventTimeout)

	if commands := controller.fake.Commands(); len(commands) != 0 {
		t.Fatalf("keepalive must not query the matrix, got %v", commands)
	}
}

func TestDisconnectRemovesSubscriber(t *testing.T) {
	controller := newTestController(t, time.Minute)

	client := controller.connectSSE(t)
	client.nextState(t)

	if count := controller.events.broker.SubscriberCount(); count != 1 {
		t.Fatalf("expected 1 subscriber, got %d", count)
	}

	client.Close()

	waitFor(t, testEventTimeout, func() bool {
		return controller.events.broker.SubscriberCount() == 0
	}, "subscriber was not removed after disconnect")
}

// Ein Client, der nichts liest, darf weder Publisher noch andere Clients
// blockieren.
func TestSlowClientDoesNotBlockOthers(t *testing.T) {
	controller := newTestController(t, time.Minute)

	slow := controller.events.Subscribe()
	defer controller.events.Unsubscribe(slow)

	fast := controller.connectSSE(t)
	fast.nextState(t)

	published := make(chan struct{})

	go func() {
		defer close(published)

		for input := 1; input <= 50; input++ {
			controller.matrix.storeOutput(1, (input%4)+1)
			controller.events.Publish()
		}
	}()

	select {
	case <-published:
	case <-time.After(testEventTimeout):
		t.Fatalf("publishing blocked on a slow client")
	}

	controller.matrix.storeOutput(1, 4)
	controller.events.Publish()

	waitForState(t, fast, "out1_in", float64(4))

	// Der Broker verwirft bei vollem Puffer das älteste Event, damit der
	// langsame Client am Ende den aktuellsten State vorfindet.
	if cap(slow.events) != sseEventBuffer {
		t.Fatalf("expected a buffer of %d events, got %d", sseEventBuffer, cap(slow.events))
	}

	var latest sseEvent
	for len(slow.events) > 0 {
		latest = <-slow.events
	}

	if latest.name != sseStateEvent {
		t.Fatalf("slow client did not buffer any state event")
	}

	var state map[string]any
	if err := json.Unmarshal(latest.data, &state); err != nil {
		t.Fatalf("decoding buffered state: %v", err)
	}
	if got := state["out1_in"]; got != float64(4) {
		t.Fatalf("slow client kept a stale event, out1_in=%v", got)
	}
}

// Der Shutdown muss offene Streams beenden.
func TestBrokerCloseEndsStreams(t *testing.T) {
	controller := newTestController(t, time.Minute)

	client := controller.connectSSE(t)
	client.nextState(t)

	controller.events.Close()

	select {
	case _, ok := <-client.frames:
		if ok {
			// Restliche Frames abwarten, bis der Kanal schließt.
			for range client.frames {
			}
		}
	case <-time.After(testEventTimeout):
		t.Fatalf("SSE stream was not closed on shutdown")
	}

	if count := controller.events.broker.SubscriberCount(); count != 0 {
		t.Fatalf("expected no subscribers after close, got %d", count)
	}
}

func TestSubscribeAfterCloseReturnsClosedChannel(t *testing.T) {
	broker := newSSEBroker()
	broker.Close()

	subscriber := broker.Subscribe()

	select {
	case _, ok := <-subscriber.events:
		if ok {
			t.Fatalf("expected a closed channel")
		}
	case <-time.After(time.Second):
		t.Fatalf("subscribing after close did not return a closed channel")
	}
}

func TestBrokerPublishIsConcurrencySafe(t *testing.T) {
	broker := newSSEBroker()
	defer broker.Close()

	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			subscriber := broker.Subscribe()
			for j := 0; j < 20; j++ {
				broker.Publish(sseEvent{name: sseStateEvent, data: []byte(`{}`)})
				select {
				case <-subscriber.events:
				default:
				}
			}
			broker.Unsubscribe(subscriber)
		}()
	}

	wg.Wait()

	if count := broker.SubscriberCount(); count != 0 {
		t.Fatalf("expected no subscribers, got %d", count)
	}
}

func TestEventsRejectsNonGET(t *testing.T) {
	controller := newTestController(t, time.Minute)

	response, err := controller.server.Client().Post(
		controller.server.URL+"/events",
		"application/json",
		strings.NewReader("{}"),
	)
	if err != nil {
		t.Fatalf("posting /events: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", response.StatusCode)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal(message)
}

func waitForState(t *testing.T, client *sseTestClient, field string, expected any) {
	t.Helper()

	deadline := time.Now().Add(testEventTimeout)
	for time.Now().Before(deadline) {
		state := client.nextState(t)
		if state[field] == expected {
			return
		}
	}

	t.Fatalf("timed out waiting for %s=%v", field, expected)
}
