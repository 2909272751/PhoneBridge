package app

import (
	"testing"
	"time"
)

func TestSessionExpiry(t *testing.T) {
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, time.Local)
	session, err := newSession(CreateSessionRequest{
		DeviceID:       "demo",
		DurationMinute: 30,
		Mode:           "control",
		RequireCode:    true,
	}, demoDevice(), "http://localhost:8787", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.AccessCode) != 6 {
		t.Fatalf("expected a 6 digit access code, got %q", session.AccessCode)
	}
	session.refresh(now.Add(29 * time.Minute))
	if session.State != "waiting" {
		t.Fatalf("session expired too early: %s", session.State)
	}
	session.refresh(now.Add(31 * time.Minute))
	if session.State != "expired" {
		t.Fatalf("expected expired, got %s", session.State)
	}
}
