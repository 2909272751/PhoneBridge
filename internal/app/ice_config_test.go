package app

import (
	"io"
	"log"
	"path/filepath"
	"testing"
)

func TestServerKeepsConfiguredSTUNServers(t *testing.T) {
	configured := []ICEServerConfig{{URLs: []string{"stun:example.test:3478"}}}
	server := New(Config{
		ListenAddress: "127.0.0.1:0",
		SettingsPath:  filepath.Join(t.TempDir(), "settings.json"),
		ICEServers:    configured,
	}, log.New(io.Discard, "", 0))
	if len(server.config.ICEServers) != 1 || server.config.ICEServers[0].URLs[0] != configured[0].URLs[0] {
		t.Fatal("configured STUN server was not retained")
	}
	if hasTURNServer(server.config.ICEServers) {
		t.Fatal("public STUN defaults must not be presented as TURN")
	}
}
