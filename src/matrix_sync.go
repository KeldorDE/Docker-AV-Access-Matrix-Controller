package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

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
	if !m.cache.initialized {
		m.cache.initialized = true
		m.cache.markChangedLocked()
	}
	if hdcpInitialized && !m.cache.hdcpInitialized {
		m.cache.hdcpInitialized = true
		m.cache.markChangedLocked()
	}
	m.cache.updatedAt = time.Now()
	m.cache.mu.Unlock()

	return nil
}

// bulkMappingCommand fragt das Routing aller Ausgänge mit einem einzigen
// Telnet-Kommando ab.
const bulkMappingCommand = "GET MP all"

// bulkMappingTimeoutLimit legt fest, nach wie vielen Timeouts in Folge das
// Sammelkommando als nicht unterstützt gilt. Ein einzelnes Timeout ist meist
// nur ein Transportproblem und darf den schnellen Poll nicht dauerhaft
// verschlechtern.
const bulkMappingTimeoutLimit = 3

// errBulkMappingTimeout meldet, dass die Matrix das Sammelkommando nicht
// rechtzeitig vollständig beantwortet hat.
var errBulkMappingTimeout = errors.New("matrix did not answer the bulk mapping command in time")

func (m *matrixConnection) bulkMappingSupported() bool {
	m.cache.mu.RLock()
	defer m.cache.mu.RUnlock()

	return !m.cache.bulkMappingUnsupported
}

func (m *matrixConnection) markBulkMappingUnsupported() {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()

	m.cache.bulkMappingUnsupported = true
}

func (m *matrixConnection) noteBulkMappingSuccess() {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()

	m.cache.bulkMappingTimeouts = 0
}

// noteBulkMappingTimeout zählt Timeouts in Folge und meldet, ob das
// Sammelkommando deswegen ab jetzt übersprungen wird.
func (m *matrixConnection) noteBulkMappingTimeout() bool {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()

	m.cache.bulkMappingTimeouts++
	if m.cache.bulkMappingTimeouts < bulkMappingTimeoutLimit {
		return false
	}

	m.cache.bulkMappingUnsupported = true
	return true
}

// RefreshRouting aktualisiert nur das Input/Output-Routing. Das ist der Wert,
// der sich bei physischen Umschaltungen am Frontpanel ändert, deshalb genügt
// dem schnellen Poll dieses eine Kommando.
func (m *matrixConnection) RefreshRouting() error {
	if m.bulkMappingSupported() {
		err := m.refreshRoutingBulk()

		switch {
		case err == nil:
			m.noteBulkMappingSuccess()
			return nil
		case errors.Is(err, errCommandUnsupported):
			// Die Matrix hat mit ihrer Welcome-Zeile geantwortet und kennt
			// das Kommando damit sicher nicht.
			log.Printf(
				"INFO Matrix does not support %q; falling back to per-output routing polls",
				bulkMappingCommand,
			)
			m.markBulkMappingUnsupported()
		case errors.Is(err, errBulkMappingTimeout):
			if m.noteBulkMappingTimeout() {
				log.Printf(
					"WARNING Matrix did not answer %q %d times in a row; falling back to per-output routing polls",
					bulkMappingCommand,
					bulkMappingTimeoutLimit,
				)
			} else {
				debugf("%s timed out; using per-output routing polls this cycle", bulkMappingCommand)
			}
		default:
			return err
		}
	}

	outputs := m.OutputCount()
	for output := 1; output <= outputs; output++ {
		if _, err := m.GetOutput(output); err != nil {
			return fmt.Errorf("refreshing output %d: %w", output, err)
		}
	}

	return nil
}

// refreshRoutingBulk wertet die mehrzeilige Antwort von "GET MP all" aus. Die
// Matrix trennt diese Zeilen laut Command Set nur mit <CR> und schließt erst
// die letzte mit <CR><LF> ab, deshalb kann eine gelesene Zeile mehrere
// Mappings enthalten.
func (m *matrixConnection) refreshRoutingBulk() error {
	outputs := m.OutputCount()
	inputs := m.InputCount()

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureConnectedLocked(); err != nil {
		return err
	}

	if err := m.writeLineLocked(bulkMappingCommand); err != nil {
		m.closeLocked()
		return fmt.Errorf("%s: %w", bulkMappingCommand, err)
	}

	mapping := make(map[int]int, outputs)
	maxLines := 2*outputs + 8

	for line := 0; len(mapping) < outputs; line++ {
		if line >= maxLines {
			// Die Matrix redet, liefert aber keine vollständige Zuordnung.
			// Der Lesepuffer ist damit nicht mehr synchron.
			m.closeLocked()
			return fmt.Errorf("%s: %w", bulkMappingCommand, errBulkMappingTimeout)
		}

		response, err := m.readLineLocked()
		if err != nil {
			m.closeLocked()

			if errors.Is(err, os.ErrDeadlineExceeded) {
				debugf(
					"%s returned %d of %d mappings before timing out",
					bulkMappingCommand,
					len(mapping),
					outputs,
				)
				return fmt.Errorf("%s: %w", bulkMappingCommand, errBulkMappingTimeout)
			}

			return fmt.Errorf("%s: %w", bulkMappingCommand, err)
		}

		if isGreetingLine(response) {
			m.setModel(parseGreetingModel(response))
			return fmt.Errorf("%s: %w", bulkMappingCommand, errCommandUnsupported)
		}

		matched := false
		for _, part := range strings.Split(response, "\r") {
			input, output, ok := parseMappingLine(part, inputs, outputs)
			if !ok {
				continue
			}

			mapping[output] = input
			matched = true
		}

		if !matched {
			m.sniffIdentityLine(response)
			debugf("Ignoring unexpected response: %q", response)
		}
	}

	for output, input := range mapping {
		m.storeOutput(output, input)
	}

	return nil
}

func (m *matrixConnection) CachedStatus() (map[string]any, bool) {
	m.cache.mu.RLock()
	defer m.cache.mu.RUnlock()

	if !m.cache.initialized {
		return nil, false
	}

	return m.cache.snapshotLocked(), true
}

// CachedState liefert denselben State wie CachedStatus plus die Revision, mit
// der Publisher erkennen, ob sich seit dem letzten Mal etwas geändert hat.
func (m *matrixConnection) CachedState() (map[string]any, uint64, bool) {
	m.cache.mu.RLock()
	defer m.cache.mu.RUnlock()

	if !m.cache.initialized {
		return nil, 0, false
	}

	return m.cache.snapshotLocked(), m.cache.revision, true
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
