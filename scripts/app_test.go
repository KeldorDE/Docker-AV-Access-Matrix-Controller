package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Der schnelle Poll darf nur ein einziges Telnet-Kommando erzeugen.
func TestRefreshRoutingUsesBulkCommand(t *testing.T) {
	fake := startFakeMatrix(t)
	matrix := newTestMatrix(t, fake)

	if err := matrix.Connect(); err != nil {
		t.Fatalf("connecting: %v", err)
	}

	fake.SwitchPhysically(3, 2)
	fake.ResetCommands()

	if err := matrix.RefreshRouting(); err != nil {
		t.Fatalf("refreshing routing: %v", err)
	}

	commands := fake.Commands()
	if len(commands) != 1 || !strings.EqualFold(commands[0], bulkMappingCommand) {
		t.Fatalf("expected a single %q command, got %v", bulkMappingCommand, commands)
	}

	matrix.cache.mu.RLock()
	output3 := matrix.cache.outputs[3]
	matrix.cache.mu.RUnlock()

	if output3 != 2 {
		t.Fatalf("expected cached out3_in=2, got %d", output3)
	}
}

// Kennt die Matrix "GET MP all" nicht, fällt der Poll auf Einzelabfragen zurück
// und wiederholt das Sammelkommando nicht.
func TestRefreshRoutingFallsBackToPerOutputPolls(t *testing.T) {
	fake := startFakeMatrix(t)
	fake.SetBulkSupported(false)

	matrix := newTestMatrix(t, fake)
	if err := matrix.Connect(); err != nil {
		t.Fatalf("connecting: %v", err)
	}

	fake.SwitchPhysically(1, 4)
	fake.ResetCommands()

	if err := matrix.RefreshRouting(); err != nil {
		t.Fatalf("refreshing routing: %v", err)
	}

	commands := fake.Commands()
	if len(commands) != 5 {
		t.Fatalf("expected 1 bulk attempt plus 4 per-output commands, got %v", commands)
	}

	fake.ResetCommands()
	if err := matrix.RefreshRouting(); err != nil {
		t.Fatalf("refreshing routing again: %v", err)
	}

	commands = fake.Commands()
	if len(commands) != 4 {
		t.Fatalf("expected 4 per-output commands after fallback, got %v", commands)
	}
	for _, command := range commands {
		if strings.EqualFold(command, bulkMappingCommand) {
			t.Fatalf("bulk command must not be retried, got %v", commands)
		}
	}

	matrix.cache.mu.RLock()
	output1 := matrix.cache.outputs[1]
	matrix.cache.mu.RUnlock()

	if output1 != 4 {
		t.Fatalf("expected cached out1_in=4, got %d", output1)
	}
}

// Ein einzelnes Timeout darf das Sammelkommando nicht dauerhaft abschalten.
func TestRefreshRoutingSurvivesTransientBulkTimeout(t *testing.T) {
	fake := startFakeMatrix(t)
	matrix := newTestMatrix(t, fake)

	if err := matrix.Connect(); err != nil {
		t.Fatalf("connecting: %v", err)
	}

	fake.SwallowBulkCommands(1)
	fake.ResetCommands()

	if err := matrix.RefreshRouting(); err != nil {
		t.Fatalf("refreshing routing after a swallowed command: %v", err)
	}

	if !matrix.bulkMappingSupported() {
		t.Fatalf("a single timeout must not disable %q", bulkMappingCommand)
	}

	fake.SwitchPhysically(2, 4)
	fake.ResetCommands()

	if err := matrix.RefreshRouting(); err != nil {
		t.Fatalf("refreshing routing: %v", err)
	}

	commands := fake.Commands()
	if len(commands) != 1 || !strings.EqualFold(commands[0], bulkMappingCommand) {
		t.Fatalf("expected the bulk command to be used again, got %v", commands)
	}

	matrix.cache.mu.RLock()
	output2 := matrix.cache.outputs[2]
	matrix.cache.mu.RUnlock()

	if output2 != 4 {
		t.Fatalf("expected cached out2_in=4, got %d", output2)
	}
}

// Erst nach mehreren Timeouts in Folge gilt das Sammelkommando als ungeeignet.
func TestRefreshRoutingDisablesBulkAfterRepeatedTimeouts(t *testing.T) {
	fake := startFakeMatrix(t)
	matrix := newTestMatrix(t, fake)

	if err := matrix.Connect(); err != nil {
		t.Fatalf("connecting: %v", err)
	}

	fake.SwallowBulkCommands(bulkMappingTimeoutLimit)

	for i := 0; i < bulkMappingTimeoutLimit; i++ {
		if err := matrix.RefreshRouting(); err != nil {
			t.Fatalf("refreshing routing (attempt %d): %v", i+1, err)
		}
	}

	if matrix.bulkMappingSupported() {
		t.Fatalf("expected %q to be disabled after %d timeouts", bulkMappingCommand, bulkMappingTimeoutLimit)
	}

	fake.ResetCommands()
	if err := matrix.RefreshRouting(); err != nil {
		t.Fatalf("refreshing routing: %v", err)
	}

	commands := fake.Commands()
	if len(commands) != 4 {
		t.Fatalf("expected 4 per-output commands, got %v", commands)
	}
}

// Der schnelle Poll darf den Cache nicht als initialisiert markieren, solange
// EDID-Werte fehlen.
func TestRefreshRoutingDoesNotInitializeCache(t *testing.T) {
	fake := startFakeMatrix(t)
	matrix := newTestMatrix(t, fake)

	if err := matrix.Connect(); err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if err := matrix.RefreshRouting(); err != nil {
		t.Fatalf("refreshing routing: %v", err)
	}

	if _, _, ok := matrix.CachedState(); ok {
		t.Fatalf("cache must stay uninitialized until a full sync ran")
	}
}

func TestCacheRevisionTracksRealChanges(t *testing.T) {
	fake := startFakeMatrix(t)
	matrix := newTestMatrix(t, fake)

	if err := matrix.Connect(); err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if err := matrix.RefreshStatus(); err != nil {
		t.Fatalf("refreshing status: %v", err)
	}

	_, first, ok := matrix.CachedState()
	if !ok {
		t.Fatalf("expected an initialized cache")
	}

	matrix.storeOutput(1, 1)
	matrix.storeEDID(1, 5)

	if _, revision, _ := matrix.CachedState(); revision != first {
		t.Fatalf("unchanged values must not bump the revision: %d -> %d", first, revision)
	}

	matrix.storeOutput(1, 2)

	_, second, _ := matrix.CachedState()
	if second == first {
		t.Fatalf("a changed value must bump the revision")
	}
}

// Die HDCP-Felder verschwinden aus dem State, wenn die Matrix HDCP nicht kennt.
func TestStatusWithoutHDCPSupport(t *testing.T) {
	fake := startFakeMatrix(t)
	fake.SetHDCPSupported(false)

	matrix := newTestMatrix(t, fake)
	if err := matrix.Connect(); err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if err := matrix.RefreshStatus(); err != nil {
		t.Fatalf("refreshing status: %v", err)
	}

	state, _, ok := matrix.CachedState()
	if !ok {
		t.Fatalf("expected an initialized cache")
	}
	if _, present := state["hdcp_in1"]; present {
		t.Fatalf("hdcp fields must be omitted when unsupported: %v", state)
	}
	if got := state["out1_in"]; got != 1 {
		t.Fatalf("expected out1_in=1, got %v", got)
	}
}

func TestParseMappingLine(t *testing.T) {
	cases := []struct {
		line   string
		input  int
		output int
		ok     bool
	}{
		{"MP hdmiin2 hdmiout1", 2, 1, true},
		{"MP in3 out4", 3, 4, true},
		{"  mp hdmiin1 hdmiout1  ", 1, 1, true},
		{"MP hdmiin9 hdmiout1", 0, 0, false},
		{"MP hdmiin1 hdmiout9", 0, 0, false},
		{"SW hdmiin1 hdmiout1", 0, 0, false},
		{"", 0, 0, false},
	}

	for _, testCase := range cases {
		input, output, ok := parseMappingLine(testCase.line, 4, 4)
		if ok != testCase.ok || input != testCase.input || output != testCase.output {
			t.Fatalf(
				"parseMappingLine(%q) = (%d, %d, %t), want (%d, %d, %t)",
				testCase.line,
				input,
				output,
				ok,
				testCase.input,
				testCase.output,
				testCase.ok,
			)
		}
	}
}

func TestEnvDurationSeconds(t *testing.T) {
	cases := []struct {
		value    string
		expected time.Duration
	}{
		{"", 7 * time.Second},
		{"60", 60 * time.Second},
		{"0.5", 500 * time.Millisecond},
		{"3s", 3 * time.Second},
		{"1m30s", 90 * time.Second},
	}

	for _, testCase := range cases {
		if testCase.value == "" {
			t.Setenv("TEST_DURATION", "")
		} else {
			t.Setenv("TEST_DURATION", testCase.value)
		}

		if got := envDurationSeconds("TEST_DURATION", 7*time.Second); got != testCase.expected {
			t.Fatalf("envDurationSeconds(%q) = %s, want %s", testCase.value, got, testCase.expected)
		}
	}
}

// Der alte Variablenname bleibt als Alias für FULL_SYNC_INTERVAL gültig.
func TestFullSyncIntervalConfiguration(t *testing.T) {
	t.Setenv(legacyStatusPollIntervalEnvar, "")
	t.Setenv("FULL_SYNC_INTERVAL", "")

	if got := fullSyncInterval(); got != defaultFullSyncInterval {
		t.Fatalf("expected default %s, got %s", defaultFullSyncInterval, got)
	}

	t.Setenv(legacyStatusPollIntervalEnvar, "90")
	if got := fullSyncInterval(); got != 90*time.Second {
		t.Fatalf("expected legacy variable to apply, got %s", got)
	}

	t.Setenv("FULL_SYNC_INTERVAL", "2m")
	if got := fullSyncInterval(); got != 2*time.Minute {
		t.Fatalf("expected FULL_SYNC_INTERVAL to win, got %s", got)
	}
}

func TestStatusPollIntervalDefault(t *testing.T) {
	t.Setenv("STATUS_POLL_INTERVAL", "")

	cfg := loadConfig()
	if cfg.statusPollInterval != defaultStatusPollInterval {
		t.Fatalf("expected default %s, got %s", defaultStatusPollInterval, cfg.statusPollInterval)
	}
	if cfg.statusPollInterval != 3*time.Second {
		t.Fatalf("STATUS_POLL_INTERVAL default must be 3s, got %s", cfg.statusPollInterval)
	}
}

// Ein langsamer Durchlauf darf die Telnet-Verbindung nicht dauerhaft belegen:
// zwischen zwei Durchläufen liegt immer das volle Intervall.
func TestRunPollLoopKeepsIntervalBetweenRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const interval = 30 * time.Millisecond

	starts := make(chan time.Time, 8)
	done := make(chan struct{})

	go func() {
		defer close(done)

		runPollLoop(ctx, interval, func() {
			select {
			case starts <- time.Now():
			default:
			}

			// Dauert länger als das Intervall.
			time.Sleep(3 * interval)
		})
	}()

	first := <-starts
	second := <-starts
	cancel()

	if gap := second.Sub(first); gap < 4*interval {
		t.Fatalf("expected at least %s between runs, got %s", 4*interval, gap)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("poll loop did not stop on context cancellation")
	}
}
