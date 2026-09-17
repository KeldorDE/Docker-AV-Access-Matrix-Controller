package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

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

	m.cache.updatedAt = time.Now()

	if m.cache.outputs[outputNumber] == input {
		return
	}

	m.cache.outputs[outputNumber] = input
	m.cache.markChangedLocked()
}

func (m *matrixConnection) storeEDID(inputNumber, edid int) {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()

	if inputNumber < 1 || inputNumber >= len(m.cache.edids) {
		return
	}

	m.cache.updatedAt = time.Now()

	if m.cache.edids[inputNumber] == edid {
		return
	}

	m.cache.edids[inputNumber] = edid
	m.cache.markChangedLocked()
}

func (m *matrixConnection) storeHDCP(inputNumber int, enabled bool) {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()

	if inputNumber < 1 || inputNumber >= len(m.cache.hdcp) {
		return
	}

	m.cache.updatedAt = time.Now()
	m.cache.hdcpUnsupported = false

	if m.cache.hdcp[inputNumber] == enabled {
		return
	}

	m.cache.hdcp[inputNumber] = enabled
	m.cache.markChangedLocked()
}

func (m *matrixConnection) markHDCPUnsupported() {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()

	m.cache.hdcpUnsupported = true

	if !m.cache.hdcpInitialized {
		return
	}

	// Die hdcp_inX-Felder verschwinden damit aus dem State.
	m.cache.hdcpInitialized = false
	m.cache.markChangedLocked()
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

// parseMappingLine zerlegt eine einzelne MP-Zeile. Die Matrix meldet je nach
// Firmware "MP hdmiin2 hdmiout1" oder "MP in2 hdmiout1".
func parseMappingLine(line string, inputs, outputs int) (int, int, bool) {
	match := mappingRe.FindStringSubmatch(strings.TrimSpace(line))
	if match == nil {
		return 0, 0, false
	}

	input, err := strconv.Atoi(match[1])
	if err != nil || input < 1 || input > inputs {
		return 0, 0, false
	}

	output, err := strconv.Atoi(match[2])
	if err != nil || output < 1 || output > outputs {
		return 0, 0, false
	}

	return input, output, true
}

// parseOutputResponse zerlegt die Antwort auf GET MP für einen Ausgang.
func (m *matrixConnection) parseOutputResponse(response string, outputNumber int) (int, bool) {
	input, output, ok := parseMappingLine(response, m.InputCount(), m.OutputCount())
	if !ok || output != outputNumber {
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
