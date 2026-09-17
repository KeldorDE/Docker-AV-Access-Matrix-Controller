package main

import (
	"fmt"
	"sync"
	"time"
)

type matrixStatusCache struct {
	mu              sync.RWMutex
	outputs         []int
	edids           []int
	hdcp            []bool
	hdcpInitialized bool
	hdcpUnsupported bool
	initialized     bool
	updatedAt       time.Time

	// bulkMappingUnsupported merkt sich, dass die Matrix "GET MP all" nicht
	// beherrscht, damit der schnelle Poll nicht bei jedem Durchlauf erneut
	// in ein Lese-Timeout läuft.
	bulkMappingUnsupported bool
	bulkMappingTimeouts    int

	// revision zählt ausschließlich echte Änderungen am veröffentlichten
	// State. Damit erkennen SSE-Publisher, ob ein Poll etwas Neues geliefert
	// hat, ohne den State selbst vergleichen zu müssen.
	revision uint64
}

// markChangedLocked meldet eine tatsächliche Änderung des Caches.
func (c *matrixStatusCache) markChangedLocked() {
	c.revision++
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
	c.markChangedLocked()
}

// snapshotLocked baut den vollständigen Controller-State. /status und der
// SSE-Stream nutzen dieselbe Funktion und damit dasselbe Statusmodell.
func (c *matrixStatusCache) snapshotLocked() map[string]any {
	result := make(map[string]any, len(c.outputs)+2*len(c.edids))

	for output := 1; output < len(c.outputs); output++ {
		result[fmt.Sprintf("out%d_in", output)] = c.outputs[output]
	}
	for input := 1; input < len(c.edids); input++ {
		result[fmt.Sprintf("edid_in%d", input)] = c.edids[input]
	}
	if c.hdcpInitialized {
		for input := 1; input < len(c.hdcp); input++ {
			result[fmt.Sprintf("hdcp_in%d", input)] = c.hdcp[input]
		}
	}

	return result
}
