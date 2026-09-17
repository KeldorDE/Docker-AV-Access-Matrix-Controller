package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

func (a *apiServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	_, ok := a.matrix.CachedStatus()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status":  "unavailable",
			"message": "matrix status cache has not been initialized yet",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}

func (a *apiServer) statusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	result, ok := a.matrix.CachedStatus()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "status_unavailable",
			"message": "matrix status cache has not been initialized yet",
		})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (a *apiServer) hdcpStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	if !a.matrix.HDCPSupported() {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error":   "hdcp_unsupported",
			"message": "matrix does not support HDCP commands",
		})
		return
	}

	result, ok := a.matrix.CachedHDCPStatus()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "status_unavailable",
			"message": "matrix HDCP status cache has not been initialized yet",
		})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (a *apiServer) deviceInfoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	if r.URL.Query().Get("refresh") == "true" {
		if err := a.matrix.RefreshDeviceInfo(); err != nil {
			log.Printf("WARNING Device info refresh failed: %v", err)
		}
	}

	info, ok := a.matrix.DeviceInfo()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "device_info_unavailable",
			"message": "device information has not been retrieved from the matrix yet",
		})
		return
	}

	writeJSON(w, http.StatusOK, info)
}

type switchRequest struct {
	Input  *int `json:"input"`
	Output *int `json:"output"`
}

func (a *apiServer) switchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	var request switchRequest
	if err := decodeJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "invalid_request",
			"message": err.Error(),
		})
		return
	}

	if request.Input == nil || request.Output == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "invalid_request",
			"message": "input and output are required",
		})
		return
	}

	response, err := a.matrix.Switch(*request.Input, *request.Output)
	if err != nil {
		status := http.StatusInternalServerError
		errorName := "matrix_error"

		if a.matrix.validateInput(*request.Input) != nil || a.matrix.validateOutput(*request.Output) != nil {
			status = http.StatusBadRequest
			errorName = "invalid_request"
		}

		writeJSON(w, status, map[string]any{
			"error":   errorName,
			"message": err.Error(),
		})
		return
	}

	// Der Cache wurde erst nach einem erfolgreichen Matrix-Kommando
	// aktualisiert, deshalb darf jetzt ein State-Event raus.
	a.events.Publish()

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"input":    *request.Input,
		"output":   *request.Output,
		"response": response,
	})
}

type edidRequest struct {
	Input *int `json:"input"`
	EDID  *int `json:"edid"`
}

func (a *apiServer) edidHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	var request edidRequest
	if err := decodeJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "invalid_request",
			"message": err.Error(),
		})
		return
	}

	if request.Input == nil || request.EDID == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "invalid_request",
			"message": "input and edid are required",
		})
		return
	}

	response, err := a.matrix.SetEDID(*request.Input, *request.EDID)
	if err != nil {
		status := http.StatusInternalServerError
		errorName := "matrix_error"

		if a.matrix.validateInput(*request.Input) != nil || validateEDID(*request.EDID) != nil {
			status = http.StatusBadRequest
			errorName = "invalid_request"
		}

		writeJSON(w, status, map[string]any{
			"error":   errorName,
			"message": err.Error(),
		})
		return
	}

	a.events.Publish()

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"input":    *request.Input,
		"edid":     *request.EDID,
		"response": response,
	})
}

// flexibleBool akzeptiert true/false, 1/0 und "on"/"off", damit Home Assistant
// REST-Switch-Templates ohne Umwege funktionieren.
type flexibleBool bool

func (b *flexibleBool) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	switch value := raw.(type) {
	case bool:
		*b = flexibleBool(value)
		return nil
	case float64:
		switch value {
		case 1:
			*b = true
			return nil
		case 0:
			*b = false
			return nil
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "on", "true", "1", "enable", "enabled", "yes":
			*b = true
			return nil
		case "off", "false", "0", "disable", "disabled", "no":
			*b = false
			return nil
		}
	}

	return fmt.Errorf("invalid boolean value: %s", strings.TrimSpace(string(data)))
}

type hdcpRequest struct {
	Input *int          `json:"input"`
	HDCP  *flexibleBool `json:"hdcp"`
}

func (a *apiServer) hdcpSwitchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	var request hdcpRequest
	if err := decodeJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "invalid_request",
			"message": err.Error(),
		})
		return
	}

	if request.Input == nil || request.HDCP == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "invalid_request",
			"message": "input and hdcp are required",
		})
		return
	}

	enabled := bool(*request.HDCP)

	response, err := a.matrix.SetHDCP(*request.Input, enabled)
	if err != nil {
		status := http.StatusInternalServerError
		errorName := "matrix_error"

		switch {
		case errors.Is(err, errCommandUnsupported):
			status = http.StatusNotImplemented
			errorName = "hdcp_unsupported"
		case a.matrix.validateInput(*request.Input) != nil:
			status = http.StatusBadRequest
			errorName = "invalid_request"
		}

		writeJSON(w, status, map[string]any{
			"error":   errorName,
			"message": err.Error(),
		})
		return
	}

	a.events.Publish()

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"input":    *request.Input,
		"hdcp":     enabled,
		"response": response,
	})
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()

	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return err
	}

	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("request body must contain exactly one JSON object")
	}

	return nil
}

// runPollLoop führt fn wiederholt aus und wartet zwischen zwei Durchläufen
// interval. Die Pause liegt bewusst zwischen den Durchläufen und nicht zwischen
// den Startzeitpunkten: Dauert ein Poll länger als das Intervall, bleibt der
