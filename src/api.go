package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type apiServer struct {
	matrix            *matrixConnection
	events            *stateNotifier
	keepaliveInterval time.Duration
}

func newAPIServer(
	matrix *matrixConnection,
	events *stateNotifier,
	keepaliveInterval time.Duration,
) *apiServer {
	if keepaliveInterval <= 0 {
		keepaliveInterval = defaultSSEKeepaliveInterval
	}

	return &apiServer{
		matrix:            matrix,
		events:            events,
		keepaliveInterval: keepaliveInterval,
	}
}

func newRouter(api *apiServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.healthHandler)
	mux.HandleFunc("/device-info", api.deviceInfoHandler)
	mux.HandleFunc("/status", api.statusHandler)
	mux.HandleFunc("/status/hdcp", api.hdcpStatusHandler)
	mux.HandleFunc("/switch", api.switchHandler)
	mux.HandleFunc("/switch/hdcp", api.hdcpSwitchHandler)
	mux.HandleFunc("/edid", api.edidHandler)
	mux.HandleFunc("/events", api.eventsHandler)

	return mux
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("ERROR writing JSON response: %v", err)
	}
}

func methodNotAllowed(w http.ResponseWriter) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
		"error": "method_not_allowed",
	})
}
