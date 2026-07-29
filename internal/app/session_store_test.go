package app

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSessionStoreRestoresOnlyLiveShare(t *testing.T) {
	now := time.Now().Round(time.Second)
	live := &Session{ID: "live", Token: "secret-live", State: "connected", ViewerState: "connected", CreatedAt: now}
	stopped := &Session{ID: "stopped", Token: "secret-stopped", State: "stopped", CreatedAt: now}
	file := filepath.Join(t.TempDir(), "sessions.json")
	if err := saveSessionStore(file, "live", map[string]*Session{"live": live, "stopped": stopped}); err != nil {
		t.Fatalf("save session store: %v", err)
	}
	sessions, tokens, activeID := loadSessionStore(file, now.Add(time.Minute))
	if activeID != "live" || sessions["live"] == nil || tokens["secret-live"] != "live" {
		t.Fatalf("live session was not restored: active=%q sessions=%v tokens=%v", activeID, sessions, tokens)
	}
	if _, found := sessions["stopped"]; found {
		t.Fatal("stopped session must not be restored")
	}
}
