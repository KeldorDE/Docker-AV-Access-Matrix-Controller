package main

import (
	"bufio"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const fakeMatrixGreeting = "Welcome to the 4KMX44-H2 Matrix!"

var (
	fakeGetMappingRe = regexp.MustCompile(`(?i)^get\s+mp\s+hdmiout(\d+)$`)
	fakeSetSwitchRe  = regexp.MustCompile(`(?i)^set\s+sw\s+hdmiin(\d+)\s+hdmiout(\d+)$`)
	fakeGetEDIDRe    = regexp.MustCompile(`(?i)^get\s+edid\s+hdmiin(\d+)$`)
	fakeSetEDIDRe    = regexp.MustCompile(`(?i)^set\s+edid\s+hdmiin(\d+)\s+(\d+)$`)
	fakeGetHDCPRe    = regexp.MustCompile(`(?i)^get\s+hdcp_s\s+hdmiin(\d+)$`)
	fakeSetHDCPRe    = regexp.MustCompile(`(?i)^set\s+hdcp_s\s+hdmiin(\d+)\s+(on|off)$`)
)

// fakeMatrix spricht das Telnet-Protokoll der AV-Access-Matrix nach, damit die
// Tests ohne echte Hardware auskommen.
type fakeMatrix struct {
	listener net.Listener

	mu            sync.Mutex
	outputs       map[int]int
	edids         map[int]int
	hdcp          map[int]bool
	commands      []string
	hdcpSupported bool
	bulkSupported bool
	bulkSilent    int

	connections map[net.Conn]struct{}
	wg          sync.WaitGroup
}

func startFakeMatrix(t *testing.T) *fakeMatrix {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("starting fake matrix: %v", err)
	}

	matrix := &fakeMatrix{
		listener:      listener,
		outputs:       map[int]int{1: 1, 2: 2, 3: 3, 4: 4},
		edids:         map[int]int{1: 5, 2: 5, 3: 5, 4: 5},
		hdcp:          map[int]bool{1: true, 2: true, 3: true, 4: true},
		hdcpSupported: true,
		bulkSupported: true,
		connections:   make(map[net.Conn]struct{}),
	}

	matrix.wg.Add(1)
	go matrix.accept()

	t.Cleanup(matrix.Stop)

	return matrix
}

func (f *fakeMatrix) accept() {
	defer f.wg.Done()

	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}

		f.mu.Lock()
		f.connections[conn] = struct{}{}
		f.mu.Unlock()

		f.wg.Add(1)
		go f.serve(conn)
	}
}

func (f *fakeMatrix) serve(conn net.Conn) {
	defer f.wg.Done()
	defer func() {
		f.mu.Lock()
		delete(f.connections, conn)
		f.mu.Unlock()
		_ = conn.Close()
	}()

	if _, err := fmt.Fprintf(conn, "%s\r\n", fakeMatrixGreeting); err != nil {
		return
	}

	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		response := f.handle(strings.TrimSpace(line))
		if response == "" {
			continue
		}

		if _, err := fmt.Fprintf(conn, "%s\r\n", response); err != nil {
			return
		}
	}
}

func (f *fakeMatrix) handle(command string) string {
	if command == "" {
		return ""
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.commands = append(f.commands, command)

	switch strings.ToUpper(command) {
	case "GET VER":
		return "4KMX44-H2 VER 1.0, ARM VER 1.0"
	case "GET IPADDR":
		return "IPADDR IP:192.168.1.4 MASK:255.255.255.0 GATE:192.168.1.1"
	case "GET IP MODE":
		return "IP MODE dhcp"
	case "GET MP ALL":
		if !f.bulkSupported {
			return fakeMatrixGreeting
		}

		// Simuliert eine Matrix, die das Kommando verschluckt.
		if f.bulkSilent > 0 {
			f.bulkSilent--
			return ""
		}

		// Das Command Set trennt die Zeilen von "GET MP all" nur mit <CR>
		// und beendet erst die letzte mit <CR><LF>.
		lines := make([]string, 0, len(f.outputs))
		for output := 1; output <= len(f.outputs); output++ {
			lines = append(lines, fmt.Sprintf("MP hdmiin%d hdmiout%d", f.outputs[output], output))
		}
		return strings.Join(lines, "\r")
	}

	if match := fakeGetMappingRe.FindStringSubmatch(command); match != nil {
		output, _ := strconv.Atoi(match[1])
		return fmt.Sprintf("MP hdmiin%d hdmiout%d", f.outputs[output], output)
	}

	if match := fakeSetSwitchRe.FindStringSubmatch(command); match != nil {
		input, _ := strconv.Atoi(match[1])
		output, _ := strconv.Atoi(match[2])
		f.outputs[output] = input
		return fmt.Sprintf("SW hdmiin%d hdmiout%d", input, output)
	}

	if match := fakeGetEDIDRe.FindStringSubmatch(command); match != nil {
		input, _ := strconv.Atoi(match[1])
		return fmt.Sprintf("EDID hdmiin%d %d", input, f.edids[input])
	}

	if match := fakeSetEDIDRe.FindStringSubmatch(command); match != nil {
		input, _ := strconv.Atoi(match[1])
		edid, _ := strconv.Atoi(match[2])
		f.edids[input] = edid
		return fmt.Sprintf("EDID hdmiin%d %d", input, edid)
	}

	if match := fakeGetHDCPRe.FindStringSubmatch(command); match != nil {
		if !f.hdcpSupported {
			return fakeMatrixGreeting
		}

		input, _ := strconv.Atoi(match[1])
		return fmt.Sprintf("HDCP_S hdmiin%d %s", input, hdcpValue(f.hdcp[input]))
	}

	if match := fakeSetHDCPRe.FindStringSubmatch(command); match != nil {
		if !f.hdcpSupported {
			return fakeMatrixGreeting
		}

		input, _ := strconv.Atoi(match[1])
		enabled := strings.EqualFold(match[2], "on")
		f.hdcp[input] = enabled
		return fmt.Sprintf("HDCP_S hdmiin%d %s", input, hdcpValue(enabled))
	}

	// Unbekannte Kommandos beantwortet die Matrix mit ihrer Welcome-Zeile.
	return fakeMatrixGreeting
}

// SwitchPhysically simuliert eine Umschaltung am Frontpanel.
func (f *fakeMatrix) SwitchPhysically(output, input int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.outputs[output] = input
}

func (f *fakeMatrix) SetBulkSupported(supported bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.bulkSupported = supported
}

// SwallowBulkCommands lässt die nächsten n "GET MP all"-Kommandos unbeantwortet.
func (f *fakeMatrix) SwallowBulkCommands(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.bulkSilent = n
}

func (f *fakeMatrix) SetHDCPSupported(supported bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.hdcpSupported = supported
}

func (f *fakeMatrix) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.commands...)
}

func (f *fakeMatrix) ResetCommands() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.commands = nil
}

func (f *fakeMatrix) Stop() {
	_ = f.listener.Close()

	f.mu.Lock()
	for conn := range f.connections {
		_ = conn.Close()
	}
	f.mu.Unlock()

	f.wg.Wait()
}

// newTestMatrix verbindet einen matrixConnection mit der Fake-Matrix.
func newTestMatrix(t *testing.T, fake *fakeMatrix) *matrixConnection {
	t.Helper()

	host, portValue, err := net.SplitHostPort(fake.listener.Addr().String())
	if err != nil {
		t.Fatalf("splitting fake matrix address: %v", err)
	}

	port, err := strconv.Atoi(portValue)
	if err != nil {
		t.Fatalf("parsing fake matrix port: %v", err)
	}

	matrix := newMatrixConnection(matrixOptions{
		host:    host,
		port:    port,
		timeout: testMatrixTimeout,
		inputs:  4,
		outputs: 4,
	})

	t.Cleanup(matrix.Close)

	return matrix
}
