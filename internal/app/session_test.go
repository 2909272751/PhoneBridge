package app

import (
	"testing"
	"time"
)

func TestNewSessionDefaultsToUnlimitedWhenDurationIsZero(t *testing.T) {
	session, err := newSession(CreateSessionRequest{DeviceID: "phone", DurationMinute: 0}, Device{ID: "phone", Model: "Test phone"}, "https://example.test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if session.ExpiresAt != nil {
		t.Fatalf("unlimited session unexpectedly expires at %v", session.ExpiresAt)
	}
	if session.StreamProfile != string(videoProfileHD) {
		t.Fatalf("default profile = %q, want %q", session.StreamProfile, videoProfileHD)
	}
}
