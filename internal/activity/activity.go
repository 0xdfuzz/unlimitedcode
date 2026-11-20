// Package activity records recent client activity shared between the control
// API and the log observer. It carries timestamps only.
package activity

import (
	"sync/atomic"
	"time"
)

// Tracker stores the last time a plugin event reached the control API.
type Tracker struct {
	lastPlugin atomic.Int64
}

// MarkPluginEvent records one plugin event at the current time.
func (t *Tracker) MarkPluginEvent() {
	if t == nil {
		return
	}
	t.lastPlugin.Store(time.Now().UnixNano())
}

// PluginEventWithin reports whether a plugin event arrived within window.
func (t *Tracker) PluginEventWithin(window time.Duration) bool {
	if t == nil || window <= 0 {
		return false
	}
	last := t.lastPlugin.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(0, last)) < window
}
