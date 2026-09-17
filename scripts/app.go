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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultMatrixHost               = "192.168.178.10"
	defaultMatrixPort               = 23
	defaultMatrixTimeout            = 2 * time.Second
	defaultMatrixCommandDelay       = 1 * time.Second
	defaultMatrixStatusPollInterval = 60 * time.Second
	defaultMatrixInputs             = 4
	defaultMatrixOutputs            = 4
	defaultHTTPHost                 = "0.0.0.0"
	defaultHTTPPort                 = 62225
	matrixManufacturer              = "AV Access"
)

type config struct {
	matrixHost               string
	matrixPort               int
	matrixTimeout            time.Duration
	matrixCommandDelay       time.Duration
	matrixStatusPollInterval time.Duration
	matrixInputs             int
	matrixOutputs            int
	matrixConfigurationURL   string
	httpHost                 string
	httpPort                 int
	logLevel                 string
}

func loadConfig() config {
	return config{
		matrixHost:               envString("HDMI_MATRIX_IP", defaultMatrixHost),
		matrixPort:               envInt("HDMI_MATRIX_PORT", defaultMatrixPort),
		matrixTimeout:            envDurationSeconds("HDMI_MATRIX_TIMEOUT", defaultMatrixTimeout),
		matrixCommandDelay:       envDurationSeconds("HDMI_MATRIX_COMMAND_DELAY", defaultMatrixCommandDelay),
		matrixStatusPollInterval: envDurationSeconds("HDMI_MATRIX_STATUS_POLL_INTERVAL", defaultMatrixStatusPollInterval),
		matrixInputs:             envInt("HDMI_MATRIX_INPUTS", 0),
		matrixOutputs:            envInt("HDMI_MATRIX_OUTPUTS", 0),
		matrixConfigurationURL:   envString("HDMI_MATRIX_CONFIGURATION_URL", ""),
		httpHost:                 envString("HTTP_HOST", defaultHTTPHost),
		httpPort:                 envInt("HTTP_PORT", defaultHTTPPort),
		logLevel:                 strings.ToUpper(envString("LOG_LEVEL", "INFO")),
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

type matrixStatusCache struct {
	mu              sync.RWMutex
	outputs         []int
	edids           []int
	hdcp            []bool
	hdcpInitialized bool
	hdcpUnsupported bool
	initialized     bool
	updatedAt       time.Time
}

// resize passt die Cache-Größe an die tatsächliche Portanzahl der Matrix an.
// Index 0 bleibt ungenutzt, damit Port-Nummern direkt als Index dienen.
func (c *matrixStatusCache) resize(inputs, outputs int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.edids) == inputs+1 && len(c.outputs) == outputs+1 {
		return
	}

	c.edids = make([]int, inputs+1)
	c.outputs = make([]int, outputs+1)
	c.hdcp = make([]bool, inputs+1)
	c.initialized = false
	c.hdcpInitialized = false
}

// deviceIdentity sammelt alles, was die Matrix über sich selbst meldet.
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

var (
	greetingRe    = regexp.MustCompile(`(?i)welcome\s+to\s+the\s+(\S+)\s+matrix`)
	modelPortsRe  = regexp.MustCompile(`(?i)mx(\d+)`)
	versionPairRe = regexp.MustCompile(`(?i)([a-z0-9][a-z0-9._\-]*)\s+ver\s+(v?\d[\w.\-]*)`)
	versionOnlyRe = regexp.MustCompile(`(?i)^ver\s+(v?\d[\w.\-]*)`)
	ipFieldRe     = regexp.MustCompile(`(?i)\b(ip|mask|gate)\s*:\s*(\d{1,3}(?:\.\d{1,3}){3})`)
	ipModeRe      = regexp.MustCompile(`(?i)^ip\s+mode\s+([a-z]+)`)
	hdcpRe        = regexp.MustCompile(`(?i)^hdcp_s\s+hdmiin(\d+)\s+(on|off|enable[d]?|disable[d]?|1|0)\b`)
	mappingRe     = regexp.MustCompile(`(?i)^mp\s+(?:hdmi)?in(\d+)\s+(?:hdmi)?out(\d+)\b`)
	idSanitizeRe  = regexp.MustCompile(`[^a-z0-9]+`)
)

// Die Matrix beantwortet unbekannte Kommandos mit ihrer Welcome-Zeile
// ("Welcome to the 4KMX44-H2 Matrix!"). Das ist gleichzeitig die einzige
// Quelle für den Modellnamen, die ohne Kommando auskommt.
func isGreetingLine(line string) bool {
	return greetingRe.MatchString(line)
}

func parseGreetingModel(line string) string {
	match := greetingRe.FindStringSubmatch(line)
	if match == nil {
		return ""
	}
	return strings.Trim(match[1], "!.,;:")
}

// portsFromModel leitet die Portanzahl aus dem Modellnamen ab:
// 4KMX44-H2 bedeutet 4 Eingänge und 4 Ausgänge.
func portsFromModel(model string) (int, int, bool) {
	match := modelPortsRe.FindStringSubmatch(model)
	if match == nil {
		return 0, 0, false
	}

	digits := match[1]
	if len(digits)%2 != 0 {
		return 0, 0, false
	}

	inputs, err := strconv.Atoi(digits[:len(digits)/2])
	if err != nil || inputs < 1 {
		return 0, 0, false
	}

	outputs, err := strconv.Atoi(digits[len(digits)/2:])
	if err != nil || outputs < 1 {
		return 0, 0, false
	}

	return inputs, outputs, true
}

type versionInfo struct {
	model    string
	firmware string
	arm      string
}

// parseVersionResponse zerlegt die Antwort auf GET VER, z. B.
// "4KMX44-H2 VER 1.0, ARM VER 1.0".
func parseVersionResponse(line string) (versionInfo, bool) {
	var info versionInfo
	found := false

	for _, match := range versionPairRe.FindAllStringSubmatch(line, -1) {
		found = true
		label, version := match[1], match[2]

		switch strings.ToUpper(label) {
		case "ARM":
			info.arm = version
		case "MCU", "MASTER", "FW", "FIRMWARE":
			info.firmware = version
		default:
			if info.model == "" {
				info.model = label
			}
			if info.firmware == "" {
				info.firmware = version
			}
		}
	}

	if !found {
		if match := versionOnlyRe.FindStringSubmatch(strings.TrimSpace(line)); match != nil {
			info.firmware = match[1]
			found = true
		}
	}

	return info, found
}

type networkInfo struct {
	ip      string
	netmask string
	gateway string
}

// parseIPAddrResponse zerlegt die Antwort auf GET IPADDR, z. B.
// "IPADDR IP:192.168.1.4 MASK:255.255.255.0 GATE:192.168.1.1".
func parseIPAddrResponse(line string) (networkInfo, bool) {
	var info networkInfo

	for _, match := range ipFieldRe.FindAllStringSubmatch(line, -1) {
		switch strings.ToUpper(match[1]) {
		case "IP":
			info.ip = match[2]
		case "MASK":
			info.netmask = match[2]
		case "GATE":
			info.gateway = match[2]
		}
	}

	return info, info.ip != ""
}

func parseIPModeResponse(line string) (string, bool) {
	match := ipModeRe.FindStringSubmatch(strings.TrimSpace(line))
	if match == nil {
		return "", false
	}
	return strings.ToLower(match[1]), true
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

func (m *matrixConnection) validateInput(number int) error {
	count := m.InputCount()
	if number < 1 || number > count {
		return fmt.Errorf("Input must be between 1 and %d", count)
	}
	return nil
}

func (m *matrixConnection) validateOutput(number int) error {
	count := m.OutputCount()
	if number < 1 || number > count {
		return fmt.Errorf("Output must be between 1 and %d", count)
	}
	return nil
}

// validateEDID prüft den gültigen Bereich für SET EDID. Die Firmware bietet
// 15 Profile an; das Command Set V1.0.0 listet veraltet nur 1-12.
func validateEDID(edid int) error {
	if edid < 1 || edid > 15 {
		return errors.New("EDID must be between 1 and 15")
	}
	return nil
}

func (m *matrixConnection) storeOutput(outputNumber, input int) {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()

	if outputNumber < 1 || outputNumber >= len(m.cache.outputs) {
		return
	}

	m.cache.outputs[outputNumber] = input
	m.cache.updatedAt = time.Now()
}

func (m *matrixConnection) storeEDID(inputNumber, edid int) {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()

	if inputNumber < 1 || inputNumber >= len(m.cache.edids) {
		return
	}

	m.cache.edids[inputNumber] = edid
	m.cache.updatedAt = time.Now()
}

func (m *matrixConnection) storeHDCP(inputNumber int, enabled bool) {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()

	if inputNumber < 1 || inputNumber >= len(m.cache.hdcp) {
		return
	}

	m.cache.hdcp[inputNumber] = enabled
	m.cache.hdcpUnsupported = false
	m.cache.updatedAt = time.Now()
}

func (m *matrixConnection) markHDCPUnsupported() {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()

	m.cache.hdcpUnsupported = true
	m.cache.hdcpInitialized = false
}

// HDCPSupported meldet false, sobald die Matrix ein HDCP-Kommando mit ihrer
// Welcome-Zeile quittiert hat.
func (m *matrixConnection) HDCPSupported() bool {
	m.cache.mu.RLock()
	defer m.cache.mu.RUnlock()

	return !m.cache.hdcpUnsupported
}

func (m *matrixConnection) Switch(inputNumber, outputNumber int) (int, error) {
	if err := m.validateInput(inputNumber); err != nil {
		return 0, err
	}
	if err := m.validateOutput(outputNumber); err != nil {
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

	for {
		response, err := m.readLineLocked()
		if err != nil {
			m.closeLocked()
			return 0, err
		}

		if input, ok := m.parseOutputResponse(response, outputNumber); ok && input == inputNumber {
			m.storeOutput(outputNumber, inputNumber)
			return inputNumber, nil
		}

		if isGreetingLine(response) {
			m.setModel(parseGreetingModel(response))
		}

		debugf("Ignoring response while verifying switch: %q", response)
	}
}

// parseOutputResponse zerlegt die Antwort auf GET MP. Die Matrix meldet je nach
// Firmware "MP hdmiin2 hdmiout1" oder "MP in2 hdmiout1".
func (m *matrixConnection) parseOutputResponse(response string, outputNumber int) (int, bool) {
	match := mappingRe.FindStringSubmatch(strings.TrimSpace(response))
	if match == nil {
		return 0, false
	}

	input, err := strconv.Atoi(match[1])
	if err != nil || input < 1 || input > m.InputCount() {
		return 0, false
	}

	output, err := strconv.Atoi(match[2])
	if err != nil || output != outputNumber {
		return 0, false
	}

	return input, true
}

func (m *matrixConnection) GetOutput(outputNumber int) (int, error) {
	if err := m.validateOutput(outputNumber); err != nil {
		return 0, err
	}

	command := fmt.Sprintf("GET MP hdmiout%d", outputNumber)
	matches := func(line string) bool {
		_, ok := m.parseOutputResponse(line, outputNumber)
		return ok
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	response, err := m.requestLocked(command, matches, false)
	if err != nil {
		return 0, err
	}

	input, ok := m.parseOutputResponse(response, outputNumber)
	if !ok {
		return 0, fmt.Errorf("unexpected matrix response: %s", response)
	}

	m.storeOutput(outputNumber, input)

	return input, nil
}

func (m *matrixConnection) SetEDID(inputNumber, edid int) (string, error) {
	if err := m.validateInput(inputNumber); err != nil {
		return "", err
	}
	if err := validateEDID(edid); err != nil {
		return "", err
	}

	command := fmt.Sprintf("SET EDID hdmiin%d %d", inputNumber, edid)
	matches := func(line string) bool {
		value, ok := parseEDIDResponse(line, inputNumber)
		return ok && value == edid
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	response, err := m.requestLocked(command, matches, false)
	if err != nil {
		return "", err
	}

	m.storeEDID(inputNumber, edid)

	return response, nil
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
	if err := m.validateInput(inputNumber); err != nil {
		return 0, err
	}

	command := fmt.Sprintf("GET EDID hdmiin%d", inputNumber)
	matches := func(line string) bool {
		_, ok := parseEDIDResponse(line, inputNumber)
		return ok
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	response, err := m.requestLocked(command, matches, false)
	if err != nil {
		return 0, err
	}

	edid, ok := parseEDIDResponse(response, inputNumber)
	if !ok {
		return 0, fmt.Errorf("unexpected matrix response: %s", response)
	}

	m.storeEDID(inputNumber, edid)

	return edid, nil
}

func hdcpValue(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

// parseHDCPResponse zerlegt die Antwort auf GET/SET HDCP_S, z. B.
// "HDCP_S hdmiin1 on".
func parseHDCPResponse(response string, inputNumber int) (bool, bool) {
	match := hdcpRe.FindStringSubmatch(strings.TrimSpace(response))
	if match == nil {
		return false, false
	}

	input, err := strconv.Atoi(match[1])
	if err != nil || input != inputNumber {
		return false, false
	}

	switch strings.ToLower(match[2]) {
	case "on", "enable", "enabled", "1":
		return true, true
	case "off", "disable", "disabled", "0":
		return false, true
	}

	return false, false
}

// SetHDCP schaltet HDCP für einen Eingang. Kennt die Matrix das Kommando
// nicht, kommt errCommandUnsupported zurück.
func (m *matrixConnection) SetHDCP(inputNumber int, enabled bool) (string, error) {
	if err := m.validateInput(inputNumber); err != nil {
		return "", err
	}

	command := fmt.Sprintf("SET HDCP_S hdmiin%d %s", inputNumber, hdcpValue(enabled))
	matches := func(line string) bool {
		value, ok := parseHDCPResponse(line, inputNumber)
		return ok && value == enabled
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	response, err := m.requestLocked(command, matches, true)
	if err != nil {
		if errors.Is(err, errCommandUnsupported) {
			m.markHDCPUnsupported()
		}
		return "", err
	}

	m.storeHDCP(inputNumber, enabled)

	return response, nil
}

func (m *matrixConnection) GetHDCP(inputNumber int) (bool, error) {
	if err := m.validateInput(inputNumber); err != nil {
		return false, err
	}

	command := fmt.Sprintf("GET HDCP_S hdmiin%d", inputNumber)
	matches := func(line string) bool {
		_, ok := parseHDCPResponse(line, inputNumber)
		return ok
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	response, err := m.requestLocked(command, matches, true)
	if err != nil {
		if errors.Is(err, errCommandUnsupported) {
			m.markHDCPUnsupported()
		}
		return false, err
	}

	enabled, ok := parseHDCPResponse(response, inputNumber)
	if !ok {
		return false, fmt.Errorf("unexpected matrix response: %s", response)
	}

	m.storeHDCP(inputNumber, enabled)

	return enabled, nil
}

// RefreshDeviceInfo liest Modell, Firmware und Netzwerkdaten von der Matrix.
func (m *matrixConnection) RefreshDeviceInfo() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	response, ok, err := m.fetchLocked("GET VER", func(line string) bool {
		_, parsed := parseVersionResponse(line)
		return parsed
	})
	if err != nil {
		return fmt.Errorf("GET VER: %w", err)
	}
	if ok {
		if version, parsed := parseVersionResponse(response); parsed {
			m.applyVersionInfo(version)
		}
	}

	response, ok, err = m.fetchLocked("GET IPADDR", func(line string) bool {
		_, parsed := parseIPAddrResponse(line)
		return parsed
	})
	if err != nil {
		return fmt.Errorf("GET IPADDR: %w", err)
	}
	if ok {
		if network, parsed := parseIPAddrResponse(response); parsed {
			m.applyNetworkInfo(network)
		}
	}

	response, ok, err = m.fetchLocked("GET IP MODE", func(line string) bool {
		_, parsed := parseIPModeResponse(line)
		return parsed
	})
	if err != nil {
		return fmt.Errorf("GET IP MODE: %w", err)
	}
	if ok {
		if mode, parsed := parseIPModeResponse(response); parsed {
			m.setIPMode(mode)
		}
	}

	m.identityMu.Lock()
	m.identity.initialized = true
	m.identity.detailsFetched = true
	m.identity.updatedAt = time.Now()
	m.identityMu.Unlock()

	return nil
}

func (m *matrixConnection) RefreshStatus() error {
	outputs := m.OutputCount()
	inputs := m.InputCount()

	for output := 1; output <= outputs; output++ {
		if _, err := m.GetOutput(output); err != nil {
			return fmt.Errorf("refreshing output %d: %w", output, err)
		}
	}

	for input := 1; input <= inputs; input++ {
		if _, err := m.GetEDID(input); err != nil {
			return fmt.Errorf("refreshing EDID input %d: %w", input, err)
		}
	}

	hdcpInitialized := false
	if m.HDCPSupported() {
		hdcpInitialized = true

		for input := 1; input <= inputs; input++ {
			if _, err := m.GetHDCP(input); err != nil {
				if errors.Is(err, errCommandUnsupported) {
					log.Printf("WARNING Matrix does not support HDCP commands")
					hdcpInitialized = false
					break
				}
				return fmt.Errorf("refreshing HDCP input %d: %w", input, err)
			}
		}
	}

	m.cache.mu.Lock()
	m.cache.initialized = true
	if hdcpInitialized {
		m.cache.hdcpInitialized = true
	}
	m.cache.updatedAt = time.Now()
	m.cache.mu.Unlock()

	return nil
}

func (m *matrixConnection) CachedStatus() (map[string]any, bool) {
	m.cache.mu.RLock()
	defer m.cache.mu.RUnlock()

	if !m.cache.initialized {
		return nil, false
	}

	result := make(map[string]any, len(m.cache.outputs)+2*len(m.cache.edids))
	for output := 1; output < len(m.cache.outputs); output++ {
		result[fmt.Sprintf("out%d_in", output)] = m.cache.outputs[output]
	}
	for input := 1; input < len(m.cache.edids); input++ {
		result[fmt.Sprintf("edid_in%d", input)] = m.cache.edids[input]
	}
	if m.cache.hdcpInitialized {
		for input := 1; input < len(m.cache.hdcp); input++ {
			result[fmt.Sprintf("hdcp_in%d", input)] = m.cache.hdcp[input]
		}
	}

	return result, true
}

func (m *matrixConnection) CachedHDCPStatus() (map[string]bool, bool) {
	m.cache.mu.RLock()
	defer m.cache.mu.RUnlock()

	if !m.cache.hdcpInitialized {
		return nil, false
	}

	result := make(map[string]bool, len(m.cache.hdcp))
	for input := 1; input < len(m.cache.hdcp); input++ {
		result[fmt.Sprintf("hdcp_in%d", input)] = m.cache.hdcp[input]
	}

	return result, true
}

type deviceInfoResponse struct {
	Model            string  `json:"model"`
	Manufacturer     string  `json:"manufacturer"`
	UniqueID         string  `json:"unique_id"`
	SWVersion        *string `json:"sw_version"`
	HWVersion        *string `json:"hw_version"`
	ARMVersion       *string `json:"arm_version"`
	ConfigurationURL string  `json:"configuration_url"`
	IPAddress        string  `json:"ip_address"`
	Netmask          *string `json:"netmask"`
	Gateway          *string `json:"gateway"`
	IPMode           *string `json:"ip_mode"`
	InputCount       int     `json:"input_count"`
	OutputCount      int     `json:"output_count"`
	Host             string  `json:"host"`
	Port             int     `json:"port"`
	UpdatedAt        *string `json:"updated_at"`
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func sanitizeID(value string) string {
	return strings.Trim(
		idSanitizeRe.ReplaceAllString(strings.ToLower(value), "-"),
		"-",
	)
}

// buildUniqueID liefert eine über Neustarts stabile ID. Das Command Set des
// 4KMX44-H2 kennt weder Seriennummer noch MAC-Adresse, deshalb bleibt nur
// Modell + konfigurierter Host.
func buildUniqueID(identity deviceIdentity, host string) string {
	model := identity.model
	if model == "" {
		model = "av-access-matrix"
	}

	return sanitizeID(model + "-" + host)
}

func buildConfigurationURL(ipAddress, override string) string {
	if override != "" {
		return override
	}
	if ipAddress == "" {
		return ""
	}
	if strings.Contains(ipAddress, ":") {
		return "http://[" + ipAddress + "]"
	}
	return "http://" + ipAddress
}

func (m *matrixConnection) DeviceInfoComplete() bool {
	m.identityMu.RLock()
	defer m.identityMu.RUnlock()

	return m.identity.detailsFetched
}

func (m *matrixConnection) DeviceInfo() (deviceInfoResponse, bool) {
	m.identityMu.RLock()
	identity := m.identity
	m.identityMu.RUnlock()

	if !identity.initialized {
		return deviceInfoResponse{}, false
	}

	ipAddress := identity.ipAddress
	if ipAddress == "" {
		ipAddress = m.host
	}

	info := deviceInfoResponse{
		Model:            identity.model,
		Manufacturer:     matrixManufacturer,
		UniqueID:         buildUniqueID(identity, m.host),
		SWVersion:        optionalString(identity.firmware),
		HWVersion:        optionalString(identity.hardware),
		ARMVersion:       optionalString(identity.armFirmware),
		ConfigurationURL: buildConfigurationURL(ipAddress, m.configurationURL),
		IPAddress:        ipAddress,
		Netmask:          optionalString(identity.netmask),
		Gateway:          optionalString(identity.gateway),
		IPMode:           optionalString(identity.ipMode),
		InputCount:       identity.inputCount,
		OutputCount:      identity.outputCount,
		Host:             m.host,
		Port:             m.port,
	}

	if !identity.updatedAt.IsZero() {
		info.UpdatedAt = optionalString(
			identity.updatedAt.UTC().Format(time.RFC3339),
		)
	}

	return info, true
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

func main() {
	cfg := loadConfig()
	debugEnabled = cfg.logLevel == "DEBUG"

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	if cfg.matrixCommandDelay < 0 {
		log.Fatalf("HDMI_MATRIX_COMMAND_DELAY must be >= 0")
	}
	if cfg.matrixStatusPollInterval <= 0 {
		log.Fatalf("HDMI_MATRIX_STATUS_POLL_INTERVAL must be > 0")
	}
	if cfg.matrixInputs < 0 || cfg.matrixOutputs < 0 {
		log.Fatalf("HDMI_MATRIX_INPUTS and HDMI_MATRIX_OUTPUTS must be >= 0")
	}

	log.Printf("INFO Starting AV Access controller")
	log.Printf("INFO Matrix: %s:%d", cfg.matrixHost, cfg.matrixPort)
	log.Printf("INFO Matrix command delay: %s", cfg.matrixCommandDelay)
	log.Printf("INFO Matrix status poll interval: %s", cfg.matrixStatusPollInterval)
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
		}
	}

	api := &apiServer{matrix: matrix}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.healthHandler)
	mux.HandleFunc("/device-info", api.deviceInfoHandler)
	mux.HandleFunc("/status", api.statusHandler)
	mux.HandleFunc("/status/hdcp", api.hdcpStatusHandler)
	mux.HandleFunc("/switch", api.switchHandler)
	mux.HandleFunc("/switch/hdcp", api.hdcpSwitchHandler)
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
		ticker := time.NewTicker(cfg.matrixStatusPollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !matrix.DeviceInfoComplete() {
					if err := matrix.RefreshDeviceInfo(); err != nil {
						log.Printf("WARNING Device info refresh failed: %v", err)
					}
				}

				debugf("Refreshing matrix status cache")
				if err := matrix.RefreshStatus(); err != nil {
					log.Printf("WARNING Matrix status refresh failed: %v", err)
					continue
				}
				debugf("Matrix status cache refreshed")
			}
		}
	}()

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
