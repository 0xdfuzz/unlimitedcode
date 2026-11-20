package activity

import (
	"testing"
	"time"
)

func TestTrackerRecent(t *testing.T) {
	var tracker *Tracker
	if tracker.PluginEventWithin(time.Minute) {
		t.Fatal("nil tracker must not report activity")
	}
	if tracker.PluginEventWithin(0) {
		t.Fatal("zero window must not report activity")
	}

	tracker = &Tracker{}
	if tracker.PluginEventWithin(time.Minute) {
		t.Fatal("empty tracker must not report activity")
	}

	tracker.MarkPluginEvent()
	if !tracker.PluginEventWithin(time.Minute) {
		t.Fatal("marked tracker must report activity")
	}
}
