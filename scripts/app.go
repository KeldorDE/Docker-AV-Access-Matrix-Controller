package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultMatrixHost    = "192.168.178.10"
	defaultMatrixPort    = 23
	defaultMatrixTimeout = 2 * time.Second
	defaultHTTPHost      = "0.0.0.0"
	defaultHTTPPort      = 62225
)

type config struct {
	matrixHost    string
	matrixPort    int
	matrixTimeout time.Duration
	httpHost      string
	httpPort      int
	logLevel      string
}

func loadConfig() config {
	return config{
		matrixHost:    envString("HDMI_MATRIX_IP", defaultMatrixHost),
		matrixPort:    envInt("HDMI_MATRIX_PORT", defaultMatrixPort),
		matrixTimeout: envDurationSeconds("HDMI_MATRIX_TIMEOUT", defaultMatrixTimeout),
		httpHost:      envString("HTTP_HOST", defaultHTTPHost),
		httpPort:      envInt("HTTP_PORT", defaultHTTPPort),
		logLevel:      strings.ToUpper(envString("LOG_LEVEL", "INFO")),
	}
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	number, err := strconv.Atoi(value)
	if err != nil {
		log.Fatalf("Invalid %s=%q: %v", name, value, err)
	}
	return number
}

func envDurationSeconds(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		log.Fatalf("Invalid %s=%q: %v", name, value, err)
	}

	return time.Duration(seconds * float64(time.Second))
}

var debugEnabled bool

func debugf(format string, args ...any) {
	if debugEnabled {
		log.Printf("DEBUG "+format, args...)
	}
}

type matrixConnection struct {
	host    string
	port    int
	timeout time.Duration

	mu     sync.Mutex
	conn   net.Conn
	reader *bufio.Reader
}

func newMatrixConnection(host string, port int, timeout time.Duration) *matrixConnection {
	return &matrixConnection{
		host:    host,
		port:    port,
		timeout: timeout,
	}
}

func (m *matrixConnection) connectLocked() error {
	m.closeLocked()

	address := net.JoinHostPort(m.host, strconv.Itoa(m.port))
	log.Printf("INFO Connecting to matrix %s", address)

	conn, err := net.DialTimeout("tcp", address, m.timeout)
	if err != nil {
		return err
	}

	m.conn = conn
	m.reader = bufio.NewReader(conn)

	// Die Matrix ist direkt nach dem TCP-Connect noch nicht bereit für
	// Kommandos. Zuerst sendet sie ihre Welcome-Zeile. Diese vollständig
	// einlesen, bevor der Controller die Verbindung als bereit betrachtet.
	greeting, err := m.readLineLocked()
	if err != nil {
		m.closeLocked()
		return fmt.Errorf("reading matrix greeting: %w", err)
	}

	debugf("Matrix greeting: %q", greeting)

	log.Printf("INFO Connected to matrix")
	return nil
}

func (m *matrixConnection) Connect() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.connectLocked()
}

func (m *matrixConnection) closeLocked() {
	if m.conn != nil {
		_ = m.conn.Close()
	}

	m.conn = nil
	m.reader = nil
}

func (m *matrixConnection) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closeLocked()
}

func (m *matrixConnection) ensureConnectedLocked() error {
	if m.conn != nil {
		return nil
	}
	return m.connectLocked()
}

func (m *matrixConnection) readLineLocked() (string, error) {
	if m.conn == nil || m.reader == nil {
		return "", errors.New("matrix connection is not open")
	}

	if err := m.conn.SetReadDeadline(time.Now().Add(m.timeout)); err != nil {
		return "", err
	}

	line, err := m.reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", errors.New("matrix closed connection")
		}
		return "", err
	}

	line = strings.TrimSpace(line)
	debugf("RX: %s", line)

	return line, nil
}

func (m *matrixConnection) writeLineLocked(command string) error {
	if m.conn == nil {
		return errors.New("matrix connection is not open")
	}

	if err := m.conn.SetWriteDeadline(time.Now().Add(m.timeout)); err != nil {
		return err
	}

	debugf("TX: %s", command)

	_, err := io.WriteString(m.conn, command+"\r\n")
	return err
}

func (m *matrixConnection) sendLocked(command string, matches func(string) bool) (string, error) {
	if err := m.ensureConnectedLocked(); err != nil {
		return "", err
	}

	if err := m.writeLineLocked(command); err != nil {
		return "", err
	}

	// Die Matrix kann noch eine ältere Antwort oder die Welcome-Zeile
	// im TCP-Puffer haben. Deshalb lesen, bis die Antwort passt.
	for {
		response, err := m.readLineLocked()
		if err != nil {
			return "", err
		}

		if matches(response) {
			return response, nil
		}

		debugf("Ignoring unexpected response: %q", response)
	}
}

func (m *matrixConnection) command(command string, matches func(string) bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	response, err := m.sendLocked(command, matches)
	if err == nil {
		return response, nil
	}

	log.Printf("WARNING Matrix connection failed: %v; reconnecting", err)

	m.closeLocked()

	if connectErr := m.connectLocked(); connectErr != nil {
		return "", connectErr
	}

	return m.sendLocked(command, matches)
}

func validateInput(number int) error {
	if number < 1 || number > 4 {
		return errors.New("Input must be between 1 and 4")
	}
	return nil
}

func validateOutput(number int) error {
	if number < 1 || number > 4 {
		return errors.New("Output must be between 1 and 4")
	}
	return nil
}

func validateEDID(edid int) error {
	if edid < 1 || edid > 15 {
		return errors.New("EDID must be between 1 and 15")
	}
	return nil
}

func (m *matrixConnection) Switch(inputNumber, outputNumber int) (int, error) {
	if err := validateInput(inputNumber); err != nil {
		return 0, err
	}
	if err := validateOutput(outputNumber); err != nil {
		return 0, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureConnectedLocked(); err != nil {
		return 0, err
	}

	setCommand := fmt.Sprintf("SET SW hdmiin%d hdmiout%d", inputNumber, outputNumber)
	if err := m.writeLineLocked(setCommand); err != nil {
		m.closeLocked()
		return 0, err
	}

	getCommand := fmt.Sprintf("GET MP hdmiout%d", outputNumber)
	if err := m.writeLineLocked(getCommand); err != nil {
		m.closeLocked()
		return 0, err
	}

	expected := strings.ToLower(
		fmt.Sprintf("MP in%d hdmiout%d", inputNumber, outputNumber),
	)

	for {
		response, err := m.readLineLocked()
		if err != nil {
			m.closeLocked()
			return 0, err
		}

		if strings.ToLower(response) == expected {
			return inputNumber, nil
		}

		debugf("Ignoring response while verifying switch: %q", response)
	}
}

func parseOutputResponse(response string, outputNumber int) (int, bool) {
	var input, output int
	n, err := fmt.Sscanf(
		strings.ToLower(response),
		"mp in%d hdmiout%d",
		&input,
		&output,
	)

	if err != nil || n != 2 || input < 1 || input > 4 || output != outputNumber {
		return 0, false
	}

	return input, true
}

func (m *matrixConnection) GetOutput(outputNumber int) (int, error) {
	if err := validateOutput(outputNumber); err != nil {
		return 0, err
	}

	command := fmt.Sprintf("GET MP hdmiout%d", outputNumber)

	response, err := m.command(
		command,
		func(line string) bool {
			_, ok := parseOutputResponse(line, outputNumber)
			return ok
		},
	)
	if err != nil {
		return 0, err
	}

	input, ok := parseOutputResponse(response, outputNumber)
	if !ok {
		return 0, fmt.Errorf("unexpected matrix response: %s", response)
	}

	return input, nil
}

func (m *matrixConnection) SetEDID(inputNumber, edid int) (string, error) {
	if err := validateInput(inputNumber); err != nil {
		return "", err
	}
	if err := validateEDID(edid); err != nil {
		return "", err
	}

	command := fmt.Sprintf("SET EDID hdmiin%d %d", inputNumber, edid)
	expected := strings.ToLower(
		fmt.Sprintf("EDID hdmiin%d %d", inputNumber, edid),
	)

	return m.command(
		command,
		func(line string) bool {
			return strings.ToLower(line) == expected
		},
	)
}

func parseEDIDResponse(response string, inputNumber int) (int, bool) {
	var input, edid int
	n, err := fmt.Sscanf(
		strings.ToLower(response),
		"edid hdmiin%d %d",
		&input,
		&edid,
	)

	if err != nil || n != 2 || input != inputNumber {
		return 0, false
	}

	return edid, true
}

func (m *matrixConnection) GetEDID(inputNumber int) (int, error) {
	if err := validateInput(inputNumber); err != nil {
		return 0, err
	}

	command := fmt.Sprintf("GET EDID hdmiin%d", inputNumber)

	response, err := m.command(
		command,
		func(line string) bool {
			_, ok := parseEDIDResponse(line, inputNumber)
			return ok
		},
	)
	if err != nil {
		return 0, err
	}

	edid, ok := parseEDIDResponse(response, inputNumber)
	if !ok {
		return 0, fmt.Errorf("unexpected matrix response: %s", response)
	}

	return edid, nil
}

type apiServer struct {
	matrix *matrixConnection
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

func (a *apiServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	_, err := a.matrix.GetOutput(1)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":  "unavailable",
			"message": err.Error(),
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

	result := make(map[string]int, 8)

	for output := 1; output <= 4; output++ {
		input, err := a.matrix.GetOutput(output)
		if err != nil {
			log.Printf("ERROR GET /status failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":   "matrix_error",
				"message": err.Error(),
			})
			return
		}

		result[fmt.Sprintf("out%d_in", output)] = input
	}

	for input := 1; input <= 4; input++ {
		edid, err := a.matrix.GetEDID(input)
		if err != nil {
			log.Printf("ERROR GET /status failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":   "matrix_error",
				"message": err.Error(),
			})
			return
		}

		result[fmt.Sprintf("edid_in%d", input)] = edid
	}

	writeJSON(w, http.StatusOK, result)
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

		if validateInput(*request.Input) != nil || validateOutput(*request.Output) != nil {
			status = http.StatusBadRequest
			errorName = "invalid_request"
		}

		writeJSON(w, status, map[string]any{
			"error":   errorName,
			"message": err.Error(),
		})
		return
	}

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

		if validateInput(*request.Input) != nil || validateEDID(*request.EDID) != nil {
			status = http.StatusBadRequest
			errorName = "invalid_request"
		}

		writeJSON(w, status, map[string]any{
			"error":   errorName,
			"message": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"input":    *request.Input,
		"edid":     *request.EDID,
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

func main() {
	cfg := loadConfig()
	debugEnabled = cfg.logLevel == "DEBUG"

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	log.Printf("INFO Starting AV Access controller")
	log.Printf("INFO Matrix: %s:%d", cfg.matrixHost, cfg.matrixPort)
	log.Printf("INFO HTTP server: %s:%d", cfg.httpHost, cfg.httpPort)

	matrix := newMatrixConnection(
		cfg.matrixHost,
		cfg.matrixPort,
		cfg.matrixTimeout,
	)

	if err := matrix.Connect(); err != nil {
		log.Printf("WARNING Initial matrix connection failed: %v", err)
	}

	api := &apiServer{matrix: matrix}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.healthHandler)
	mux.HandleFunc("/status", api.statusHandler)
	mux.HandleFunc("/switch", api.switchHandler)
	mux.HandleFunc("/edid", api.edidHandler)

	server := &http.Server{
		Addr:              net.JoinHostPort(cfg.httpHost, strconv.Itoa(cfg.httpPort)),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	go func() {
		<-ctx.Done()

		log.Printf("INFO Stopping controller")

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
