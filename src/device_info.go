package main

import (
	"strings"
	"time"
)

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
