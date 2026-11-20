package masque

import (
	"testing"
	"time"
)

func TestNormalizePrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2a09:bac5:b93:274b::3ea:4b", "2a09:bac5:b93:274b::/64"},
		{"2a09:bac5:b93:274b:1:2:3:4", "2a09:bac5:b93:274b::/64"},
		{"104.28.239.134", "104.28.239.134"},
	}
	for _, tc := range cases {
		if got := NormalizePrefix(tc.in); got != tc.want {
			t.Fatalf("NormalizePrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNextResetUTC(t *testing.T) {
	before := time.Date(2026, 11, 20, 0, 30, 0, 0, time.UTC)
	got := nextResetUTC(before)
	want := time.Date(2026, 11, 20, 1, 5, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("before reset: got %s, want %s", got, want)
	}

	after := time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)
	got = nextResetUTC(after)
	want = time.Date(2026, 11, 21, 1, 5, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("after reset: got %s, want %s", got, want)
	}
}
