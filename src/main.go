package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func runPollLoop(ctx context.Context, interval time.Duration, poll func()) {
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		poll()

		timer.Reset(interval)
	}
}

func main() {
	cfg := loadConfig()
	debugEnabled = cfg.logLevel == "DEBUG"

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	if cfg.matrixCommandDelay < 0 {
		log.Fatalf("HDMI_MATRIX_COMMAND_DELAY must be >= 0")
	}
	if cfg.statusPollInterval <= 0 {
		log.Fatalf("STATUS_POLL_INTERVAL must be > 0")
	}
	if cfg.fullSyncInterval <= 0 {
		log.Fatalf("FULL_SYNC_INTERVAL must be > 0")
	}
	if cfg.sseKeepaliveInterval <= 0 {
		log.Fatalf("SSE_KEEPALIVE_INTERVAL must be > 0")
	}
	if cfg.matrixInputs < 0 || cfg.matrixOutputs < 0 {
		log.Fatalf("HDMI_MATRIX_INPUTS and HDMI_MATRIX_OUTPUTS must be >= 0")
	}

	log.Printf("INFO Starting AV Access controller")
	log.Printf("INFO Matrix: %s:%d", cfg.matrixHost, cfg.matrixPort)
	log.Printf("INFO Matrix command delay: %s", cfg.matrixCommandDelay)
	log.Printf("INFO Matrix status poll interval: %s", cfg.statusPollInterval)
	log.Printf("INFO Matrix full sync interval: %s", cfg.fullSyncInterval)
	log.Printf("INFO HTTP server: %s:%d", cfg.httpHost, cfg.httpPort)

	matrix := newMatrixConnection(matrixOptions{
		host:             cfg.matrixHost,
		port:             cfg.matrixPort,
		timeout:          cfg.matrixTimeout,
		commandDelay:     cfg.matrixCommandDelay,
		inputs:           cfg.matrixInputs,
		outputs:          cfg.matrixOutputs,
		configurationURL: cfg.matrixConfigurationURL,
	})

	events := newStateNotifier(matrix)

	if err := matrix.Connect(); err != nil {
		log.Printf("WARNING Initial matrix connection failed: %v", err)
	} else {
		if err := matrix.RefreshDeviceInfo(); err != nil {
			log.Printf("WARNING Initial device info refresh failed: %v", err)
		} else if info, ok := matrix.DeviceInfo(); ok {
			log.Printf(
				"INFO Matrix identified: model=%q inputs=%d outputs=%d unique_id=%q",
				info.Model,
				info.InputCount,
				info.OutputCount,
				info.UniqueID,
			)
		}

		if err := matrix.RefreshStatus(); err != nil {
			log.Printf("WARNING Initial matrix status refresh failed: %v", err)
		} else {
			log.Printf("INFO Initial matrix status cache populated")
			events.Publish()
		}
	}

	api := newAPIServer(matrix, events, cfg.sseKeepaliveInterval)

	server := &http.Server{
		Addr:              net.JoinHostPort(cfg.httpHost, strconv.Itoa(cfg.httpPort)),
		Handler:           newRouter(api),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	// Schneller Poll: fragt nur das Routing ab und erkennt damit physische
	// Umschaltungen an der Matrix innerhalb weniger Sekunden.
	go runPollLoop(ctx, cfg.statusPollInterval, func() {
		debugf("Polling matrix routing")
		if err := matrix.RefreshRouting(); err != nil {
			log.Printf("WARNING Matrix routing poll failed: %v", err)
			return
		}

		events.Publish()
	})

	// Vollständiger Sync: gleicht zusätzlich EDID und HDCP ab.
	go runPollLoop(ctx, cfg.fullSyncInterval, func() {
		if !matrix.DeviceInfoComplete() {
			if err := matrix.RefreshDeviceInfo(); err != nil {
				log.Printf("WARNING Device info refresh failed: %v", err)
			}
		}

		debugf("Refreshing matrix status cache")
		if err := matrix.RefreshStatus(); err != nil {
			log.Printf("WARNING Matrix status refresh failed: %v", err)
			return
		}
		debugf("Matrix status cache refreshed")

		events.Publish()
	})

	go func() {
		<-ctx.Done()

		log.Printf("INFO Stopping controller")

		// Offene SSE-Streams zuerst beenden, sonst wartet Shutdown auf
		// Verbindungen, die nie von selbst idle werden.
		events.Close()

		shutdownCtx, cancel := context.WithTimeout(
			context.Background(),
			2*time.Second,
		)
		defer cancel()

		_ = server.Shutdown(shutdownCtx)
		matrix.Close()
	}()

	log.Printf("INFO Controller ready")

	err := server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP server failed: %v", err)
	}

	log.Printf("INFO Controller stopped")
}
