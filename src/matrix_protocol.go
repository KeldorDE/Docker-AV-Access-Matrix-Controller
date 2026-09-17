package main

import (
	"regexp"
	"strconv"
	"strings"
)

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
