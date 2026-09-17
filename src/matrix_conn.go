package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type deviceIdentity struct {
	model          string
	firmware       string
	armFirmware    string
	hardware       string
	ipAddress      string
	netmask        string
	gateway        string
	ipMode         string
	inputCount     int
	outputCount    int
	initialized    bool
	detailsFetched bool
	updatedAt      time.Time
}

type matrixOptions struct {
	host             string
	port             int
	timeout          time.Duration
	commandDelay     time.Duration
	inputs           int
	outputs          int
	configurationURL string
}

type matrixConnection struct {
	host         string
	port         int
	timeout      time.Duration
	commandDelay time.Duration

	forcedInputs     int
	forcedOutputs    int
	configurationURL string

	mu            sync.Mutex
	conn          net.Conn
	reader        *bufio.Reader
	lastCommandAt time.Time

	identityMu sync.RWMutex
	identity   deviceIdentity

	cache matrixStatusCache
}

func newMatrixConnection(options matrixOptions) *matrixConnection {
	inputs := options.inputs
	if inputs <= 0 {
		inputs = defaultMatrixInputs
	}

	outputs := options.outputs
	if outputs <= 0 {
		outputs = defaultMatrixOutputs
	}

	m := &matrixConnection{
		host:             options.host,
		port:             options.port,
		timeout:          options.timeout,
		commandDelay:     options.commandDelay,
		forcedInputs:     options.inputs,
		forcedOutputs:    options.outputs,
		configurationURL: options.configurationURL,
	}

	m.identity.inputCount = inputs
	m.identity.outputCount = outputs
	m.cache.resize(inputs, outputs)

	return m
}

func (m *matrixConnection) InputCount() int {
	m.identityMu.RLock()
	defer m.identityMu.RUnlock()

	return m.identity.inputCount
}

func (m *matrixConnection) OutputCount() int {
	m.identityMu.RLock()
	defer m.identityMu.RUnlock()

	return m.identity.outputCount
}

// applyPortCounts übernimmt eine erkannte Portanzahl, sofern sie nicht per
// Umgebungsvariable fest vorgegeben wurde.
func (m *matrixConnection) applyPortCounts(inputs, outputs int) {
	if m.forcedInputs > 0 {
		inputs = m.forcedInputs
	}
	if m.forcedOutputs > 0 {
		outputs = m.forcedOutputs
	}
	if inputs <= 0 || outputs <= 0 {
		return
	}

	m.identityMu.Lock()
	changed := m.identity.inputCount != inputs || m.identity.outputCount != outputs
	m.identity.inputCount = inputs
	m.identity.outputCount = outputs
	m.identityMu.Unlock()

	if !changed {
		return
	}

	log.Printf("INFO Matrix port layout: %d inputs x %d outputs", inputs, outputs)
	m.cache.resize(inputs, outputs)
}

func (m *matrixConnection) setModel(model string) {
	if model == "" {
		return
	}

	m.identityMu.Lock()
	unchanged := m.identity.model == model
	m.identity.model = model
	m.identity.initialized = true
	m.identity.updatedAt = time.Now()
	m.identityMu.Unlock()

	if unchanged {
		return
	}

	if inputs, outputs, ok := portsFromModel(model); ok {
		m.applyPortCounts(inputs, outputs)
	}
}

func (m *matrixConnection) applyVersionInfo(version versionInfo) {
	m.identityMu.Lock()
	if version.firmware != "" {
		m.identity.firmware = version.firmware
	}
	if version.arm != "" {
		m.identity.armFirmware = version.arm
	}
	m.identity.initialized = true
	m.identity.updatedAt = time.Now()
	m.identityMu.Unlock()

	m.setModel(version.model)
}

func (m *matrixConnection) applyNetworkInfo(network networkInfo) {
	m.identityMu.Lock()
	defer m.identityMu.Unlock()

	if network.ip != "" {
		m.identity.ipAddress = network.ip
	}
	if network.netmask != "" {
		m.identity.netmask = network.netmask
	}
	if network.gateway != "" {
		m.identity.gateway = network.gateway
	}

	m.identity.initialized = true
	m.identity.updatedAt = time.Now()
}

func (m *matrixConnection) setIPMode(mode string) {
	m.identityMu.Lock()
	defer m.identityMu.Unlock()

	m.identity.ipMode = mode
	m.identity.initialized = true
	m.identity.updatedAt = time.Now()
}

// sniffIdentityLine wertet Zeilen aus, die nicht zur erwarteten Antwort passen.
// Die Matrix schiebt Versions- und Netzwerkinfos gelegentlich nachträglich nach.
func (m *matrixConnection) sniffIdentityLine(line string) {
	if version, ok := parseVersionResponse(line); ok {
		m.applyVersionInfo(version)
		return
	}

	if network, ok := parseIPAddrResponse(line); ok {
		m.applyNetworkInfo(network)
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

	m.setModel(parseGreetingModel(greeting))

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

	if m.commandDelay > 0 && !m.lastCommandAt.IsZero() {
		elapsed := time.Since(m.lastCommandAt)
		if elapsed < m.commandDelay {
			wait := m.commandDelay - elapsed
			debugf("Waiting %s before next matrix command", wait)
			time.Sleep(wait)
		}
	}

	if err := m.conn.SetWriteDeadline(time.Now().Add(m.timeout)); err != nil {
		return err
	}

	debugf("TX: %s", command)

	_, err := io.WriteString(m.conn, command+"\r\n")
	if err == nil {
		m.lastCommandAt = time.Now()
	}
	return err
}

var errCommandUnsupported = errors.New("matrix does not support this command")

func (m *matrixConnection) sendLocked(command string, matches func(string) bool) (string, error) {
	return m.sendLockedOpts(command, matches, false)
}

// sendLockedOpts sendet ein Kommando und liest so lange Zeilen, bis die Antwort
// passt. Mit failOnGreeting wird die Welcome-Zeile als "Kommando unbekannt"
// gewertet, statt bis zum Lese-Timeout zu warten.
func (m *matrixConnection) sendLockedOpts(
	command string,
	matches func(string) bool,
	failOnGreeting bool,
) (string, error) {
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

		if isGreetingLine(response) {
			m.setModel(parseGreetingModel(response))

			if failOnGreeting {
				return "", fmt.Errorf("%s: %w", command, errCommandUnsupported)
			}
		} else {
			m.sniffIdentityLine(response)
		}

		debugf("Ignoring unexpected response: %q", response)
	}
}

// requestLocked kapselt das Reconnect-Retry, das alle Kommandos benötigen.
func (m *matrixConnection) requestLocked(
	command string,
	matches func(string) bool,
	failOnGreeting bool,
) (string, error) {
	response, err := m.sendLockedOpts(command, matches, failOnGreeting)
	if err == nil {
		return response, nil
	}

	if errors.Is(err, errCommandUnsupported) {
		return "", err
	}

	log.Printf("WARNING Matrix connection failed: %v; reconnecting", err)

	m.closeLocked()

	if connectErr := m.connectLocked(); connectErr != nil {
		return "", connectErr
	}

	return m.sendLockedOpts(command, matches, failOnGreeting)
}

// fetchLocked führt ein optionales Abfrage-Kommando aus. Beantwortet die Matrix
// es mit der Welcome-Zeile, gilt es als nicht unterstützt und nicht als Fehler.
func (m *matrixConnection) fetchLocked(
	command string,
	matches func(string) bool,
) (string, bool, error) {
	response, err := m.requestLocked(command, matches, true)
	if err == nil {
		return response, true, nil
	}

	if errors.Is(err, errCommandUnsupported) {
		debugf("Matrix does not support %q", command)
		return "", false, nil
	}

	return "", false, err
}
