package protocol

import (
	"sync/atomic"
)

// AdaptivePoolManager tracks active connections for load monitoring
type AdaptivePoolManager struct {
	activeConnections atomic.Int64
}

var globalAdaptiveManager = &AdaptivePoolManager{}

func RegisterConnection() {
	globalAdaptiveManager.activeConnections.Add(1)
}

func UnregisterConnection() {
	globalAdaptiveManager.activeConnections.Add(-1)
}

func GetActiveConnections() int64 {
	return globalAdaptiveManager.activeConnections.Load()
}
