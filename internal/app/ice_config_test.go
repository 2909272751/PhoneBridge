package app

import (
	"io"
	"log"
	"path/filepath"
	"strings"
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

func TestWebRTCTransportReason(t *testing.T) {
	cases := []struct {
		name       string
		active     bool
		fresh      bool
		servers    []ICEServerConfig
		contains   []string
		notContain []string
	}{
		{
			name: "fresh UDP mapping", active: true, fresh: true,
			contains: []string{"启用"},
		},
		{
			name: "recovered UDP mapping", active: true, fresh: false,
			contains: []string{"恢复"},
		},
		{
			name: "TURN fallback", active: false, fresh: false,
			servers:  []ICEServerConfig{{URLs: []string{"turn:example.com:3478"}}},
			contains: []string{"TURN"},
		},
		{
			name: "no realtime path", active: false, fresh: false,
			contains: []string{"兼容画面", "未配置 TURN"},
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			reason := webrtcTransportReason(item.active, item.fresh, item.servers)
			for _, want := range item.contains {
				if !strings.Contains(reason, want) {
					t.Fatalf("reason %q should contain %q", reason, want)
				}
			}
			for _, unwanted := range item.notContain {
				if strings.Contains(reason, unwanted) {
					t.Fatalf("reason %q should not contain %q", reason, unwanted)
				}
			}
		})
	}
}
