package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublicAccessManualURL(t *testing.T) {
	manager := NewPublicAccessManager(filepath.Join(t.TempDir(), "settings.json"), "", 8787, nil)
	snapshot, err := manager.Save(context.Background(), PublicAccessSettings{
		Source:    publicAccessManual,
		ManualURL: "https://share.example.test/",
	})
	if err != nil {
		t.Fatalf("save manual public URL: %v", err)
	}
	if snapshot.EffectiveURL != "https://share.example.test" || snapshot.State != "manual-ready" {
		t.Fatalf("unexpected manual snapshot: %#v", snapshot)
	}
}

func TestPublicAccessSyncs88FRPAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var data any
		switch request.URL.Path {
		case "/api/instances":
			data = []map[string]any{{"id": "home", "name": "Home"}}
		case "/api/instances/home/tunnels":
			data = map[string]any{"tunnels": []map[string]any{{"name": "phonebridge", "type": "tcp", "localPort": 8787, "remotePort": 19080, "enabled": true}}}
		case "/api/instances/home/config":
			data = map[string]any{"configText": "serverAddr = \"198.51.100.7\"\nserverPort = 7000\n"}
		default:
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": data})
	}))
	defer server.Close()

	manager := NewPublicAccessManager(filepath.Join(t.TempDir(), "settings.json"), "", 8787, nil)
	snapshot, err := manager.Save(context.Background(), PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: server.URL,
		FRPInstanceID: "home",
		FRPTunnelName: "phonebridge",
		FRPScheme:     "https",
		AutoSync:      true,
	})
	if err != nil {
		t.Fatalf("sync 88FRP: %v", err)
	}
	if snapshot.EffectiveURL != "https://198.51.100.7:19080" || snapshot.State != "frp-ready" {
		t.Fatalf("unexpected 88FRP snapshot: %#v", snapshot)
	}
	session, err := newSession(CreateSessionRequest{}, Device{}, snapshot.EffectiveURL, time.Now())
	if err != nil {
		t.Fatalf("create session from synced address: %v", err)
	}
	if !strings.HasPrefix(session.ShareURL, snapshot.EffectiveURL+"/s/") {
		t.Fatalf("share URL did not use synchronized address: %q", session.ShareURL)
	}
}

func writeSettingsFile(t *testing.T, settings PublicAccessSettings, probeID string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	content, err := json.MarshalIndent(publicAccessFile{Settings: settings, ProbeID: probeID}, "", "  ")
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return path
}

func TestPublicAccessPersistsLastSuccessURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var data any
		switch request.URL.Path {
		case "/api/instances":
			data = []map[string]any{{"id": "home", "name": "Home"}}
		case "/api/instances/home/tunnels":
			data = map[string]any{"tunnels": []map[string]any{{"name": "phonebridge", "type": "tcp", "localPort": 8787, "remotePort": 19080, "enabled": true}}}
		case "/api/instances/home/config":
			data = map[string]any{"configText": "serverAddr = \"198.51.100.7\"\n"}
		default:
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": data})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "settings.json")
	manager := NewPublicAccessManager(path, "", 8787, nil)
	snapshot, err := manager.Save(context.Background(), PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: server.URL,
		FRPInstanceID: "home",
		FRPTunnelName: "phonebridge",
		FRPScheme:     "https",
		AutoSync:      true,
	})
	if err != nil {
		t.Fatalf("sync 88FRP: %v", err)
	}
	if snapshot.State != "frp-ready" || snapshot.EffectiveURL != "https://198.51.100.7:19080" {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var saved publicAccessFile
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	if saved.Settings.FRPLastSuccessURL != "https://198.51.100.7:19080" {
		t.Fatalf("last success URL was not persisted: %q", saved.Settings.FRPLastSuccessURL)
	}
	if saved.ProbeID == "" || saved.ProbeID != manager.ProbeID() {
		t.Fatalf("local probe ID was not persisted (saved %q, live %q)", saved.ProbeID, manager.ProbeID())
	}
}

func TestPublicAccessRestoresLastSuccessURL(t *testing.T) {
	const probe = "aabbccddeeff00112233445566778899"
	path := writeSettingsFile(t, PublicAccessSettings{
		Source:            publicAccess88FRP,
		FRPServiceURL:     "http://127.0.0.1:8801",
		FRPInstanceID:     "home",
		FRPTunnelName:     "phonebridge",
		FRPScheme:         "https",
		FRPLastSuccessURL: "https://198.51.100.7:19080",
	}, probe)
	manager := NewPublicAccessManager(path, "", 8787, nil)
	if manager.EffectiveURL() != "https://198.51.100.7:19080" {
		t.Fatalf("last successful URL was not restored: %q", manager.EffectiveURL())
	}
	if manager.ProbeID() != probe {
		t.Fatalf("local probe ID was not restored: got %q want %q", manager.ProbeID(), probe)
	}
}

func TestPublicAccessSyncFailureKeepsVerifiedURL(t *testing.T) {
	const probe = "11223344556677889900aabbccddeeff"
	health := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{"app": "PhoneBridge", "probeId": probe})
			return
		}
		http.NotFound(writer, request)
	}))
	defer health.Close()
	management := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer management.Close()

	path := writeSettingsFile(t, PublicAccessSettings{
		Source:            publicAccess88FRP,
		FRPServiceURL:     management.URL,
		FRPInstanceID:     "home",
		FRPTunnelName:     "phonebridge",
		FRPScheme:         "http",
		FRPLastSuccessURL: health.URL,
	}, probe)
	manager := NewPublicAccessManager(path, "", 8787, nil)
	snapshot, err := manager.Sync(context.Background())
	if err == nil {
		t.Fatal("expected a sync failure from the management API")
	}
	if snapshot.State != "frp-stale-ready" {
		t.Fatalf("want frp-stale-ready, got %q", snapshot.State)
	}
	if snapshot.EffectiveURL != health.URL {
		t.Fatalf("verified URL was not retained: %q", snapshot.EffectiveURL)
	}
	if !strings.Contains(snapshot.Message, "公网可用") {
		t.Fatalf("message should say the public URL is usable: %q", snapshot.Message)
	}
}

func TestPublicAccessSaveKeepsVerifiedURLWhenFormOmitsRecoveryState(t *testing.T) {
	const probe = "11223344556677889900aabbccddeeff"
	health := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"app": "PhoneBridge", "probeId": probe})
			return
		}
		http.NotFound(writer, request)
	}))
	defer health.Close()
	management := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer management.Close()

	path := writeSettingsFile(t, PublicAccessSettings{
		Source:            publicAccess88FRP,
		FRPServiceURL:     management.URL,
		FRPInstanceID:     "home",
		FRPTunnelName:     "phonebridge",
		FRPScheme:         "http",
		FRPLastSuccessURL: health.URL,
	}, probe)
	manager := NewPublicAccessManager(path, "", 8787, nil)
	snapshot, err := manager.Save(context.Background(), PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: management.URL,
		FRPInstanceID: "home",
		FRPTunnelName: "phonebridge",
		FRPScheme:     "http",
		AutoSync:      true,
	})
	if err == nil {
		t.Fatal("expected a sync failure from the management API")
	}
	if snapshot.State != "frp-stale-ready" || snapshot.EffectiveURL != health.URL {
		t.Fatalf("web-form save lost recoverable public URL: %#v", snapshot)
	}
}

func TestPublicAccessSyncFailureUnverifiedURL(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			writer.Header().Set("Content-Type", "application/json")
			// Mismatched probe ID: this is not the current PhoneBridge instance.
			_ = json.NewEncoder(writer).Encode(map[string]any{"app": "PhoneBridge", "probeId": "someone-else"})
			return
		}
		http.NotFound(writer, request)
	}))
	defer health.Close()
	management := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer management.Close()

	path := writeSettingsFile(t, PublicAccessSettings{
		Source:            publicAccess88FRP,
		FRPServiceURL:     management.URL,
		FRPInstanceID:     "home",
		FRPTunnelName:     "phonebridge",
		FRPScheme:         "http",
		FRPLastSuccessURL: health.URL,
	}, "aabbccddeeff00112233445566778899")
	manager := NewPublicAccessManager(path, "", 8787, nil)
	snapshot, err := manager.Sync(context.Background())
	if err == nil {
		t.Fatal("expected a sync failure from the management API")
	}
	if snapshot.State != "frp-unreachable" {
		t.Fatalf("want frp-unreachable, got %q", snapshot.State)
	}
	if snapshot.EffectiveURL != health.URL {
		t.Fatalf("unreachable URL should be retained for diagnostics: %q", snapshot.EffectiveURL)
	}
}

func TestPublicAccessSyncFailureWithoutOldURL(t *testing.T) {
	management := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer management.Close()

	path := writeSettingsFile(t, PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: management.URL,
		FRPInstanceID: "home",
		FRPTunnelName: "phonebridge",
		FRPScheme:     "http",
	}, "aabbccddeeff00112233445566778899")
	manager := NewPublicAccessManager(path, "", 8787, nil)
	snapshot, err := manager.Sync(context.Background())
	if err == nil {
		t.Fatal("expected a sync failure from the management API")
	}
	if snapshot.State != "frp-error" {
		t.Fatalf("want frp-error without an old URL, got %q", snapshot.State)
	}
}

func udpDiscoveryServer(t *testing.T, tunnels []map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var data any
		switch request.URL.Path {
		case "/api/instances":
			data = []map[string]any{{"id": "home", "name": "Home"}}
		case "/api/instances/home/tunnels":
			data = map[string]any{"tunnels": tunnels}
		case "/api/instances/home/config":
			data = map[string]any{"configText": "serverAddr = \"203.0.113.9\"\n"}
		default:
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": data})
	}))
	return server
}

func TestPublicAccessPersistsDiscoveredUDPForward(t *testing.T) {
	server := udpDiscoveryServer(t, []map[string]any{
		{"name": "udp-video", "type": "udp", "localPort": 3478, "remotePort": 26188, "enabled": true},
	})
	defer server.Close()

	path := filepath.Join(t.TempDir(), "settings.json")
	manager := NewPublicAccessManager(path, "", 8787, nil)
	if _, err := manager.Save(context.Background(), PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: server.URL,
		FRPInstanceID: "home",
		FRPScheme:     "http",
	}); err != nil {
		t.Fatalf("save 88FRP settings: %v", err)
	}
	host, port, fresh, err := manager.DiscoverUDPForward(context.Background(), 3478)
	if err != nil {
		t.Fatalf("discover UDP forward: %v", err)
	}
	if host != "203.0.113.9" || port != 26188 || !fresh {
		t.Fatalf("unexpected UDP forward: %s:%d fresh=%v", host, port, fresh)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var saved publicAccessFile
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	mapping := saved.Settings.FRPUDPForward
	if mapping == nil || mapping.PublicHost != "203.0.113.9" || mapping.PublicPort != 26188 || mapping.LocalICEPort != 3478 || mapping.TunnelName != "udp-video" || mapping.DiscoveredAt.IsZero() {
		t.Fatalf("UDP mapping was not persisted: %#v", mapping)
	}
}

func TestDiscoverUDPForwardRestoresPersistedMappingOnFailure(t *testing.T) {
	management := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer management.Close()

	path := writeSettingsFile(t, PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: management.URL,
		FRPInstanceID: "home",
		FRPTunnelName: "phonebridge",
		FRPScheme:     "http",
		FRPUDPForward: &UDPForwardMapping{PublicHost: "203.0.113.9", PublicPort: 26188, LocalICEPort: 3478, TunnelName: "udp-video", DiscoveredAt: time.Now().Add(-time.Hour)},
	}, "aabbccddeeff00112233445566778899")
	manager := NewPublicAccessManager(path, "", 8787, nil)
	host, port, fresh, err := manager.DiscoverUDPForward(context.Background(), 3478)
	if err != nil {
		t.Fatalf("discover with persisted mapping: %v", err)
	}
	if host != "203.0.113.9" || port != 26188 || fresh {
		t.Fatalf("want stale persisted mapping, got %s:%d fresh=%v", host, port, fresh)
	}
}

func TestDiscoverUDPForwardPrefersFreshMapping(t *testing.T) {
	server := udpDiscoveryServer(t, []map[string]any{
		{"name": "udp-video", "type": "udp", "localPort": 3478, "remotePort": 30000, "enabled": true},
	})
	defer server.Close()

	path := writeSettingsFile(t, PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: server.URL,
		FRPInstanceID: "home",
		FRPScheme:     "http",
		FRPUDPForward: &UDPForwardMapping{PublicHost: "old.example.test", PublicPort: 11111, LocalICEPort: 3478, DiscoveredAt: time.Now().Add(-time.Hour)},
	}, "aabbccddeeff00112233445566778899")
	manager := NewPublicAccessManager(path, "", 8787, nil)
	host, port, fresh, err := manager.DiscoverUDPForward(context.Background(), 3478)
	if err != nil {
		t.Fatalf("discover with fresh API: %v", err)
	}
	if host != "203.0.113.9" || port != 30000 || !fresh {
		t.Fatalf("want fresh mapping over persisted, got %s:%d fresh=%v", host, port, fresh)
	}
}

func TestDiscoverUDPForwardFailsWithoutMapping(t *testing.T) {
	management := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer management.Close()

	path := writeSettingsFile(t, PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: management.URL,
		FRPInstanceID: "home",
		FRPScheme:     "http",
	}, "aabbccddeeff00112233445566778899")
	manager := NewPublicAccessManager(path, "", 8787, nil)
	if _, _, _, err := manager.DiscoverUDPForward(context.Background(), 3478); err == nil {
		t.Fatal("expected an error when neither fresh discovery nor a persisted mapping is available")
	}
}

func TestDiscoverUDPForwardRejectsMappingForOtherLocalPort(t *testing.T) {
	management := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer management.Close()

	path := writeSettingsFile(t, PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: management.URL,
		FRPInstanceID: "home",
		FRPScheme:     "http",
		FRPUDPForward: &UDPForwardMapping{PublicHost: "203.0.113.9", PublicPort: 26188, LocalICEPort: 40000, DiscoveredAt: time.Now().Add(-time.Hour)},
	}, "aabbccddeeff00112233445566778899")
	manager := NewPublicAccessManager(path, "", 8787, nil)
	if _, _, _, err := manager.DiscoverUDPForward(context.Background(), 3478); err == nil {
		t.Fatal("expected an error when the persisted mapping targets a different local ICE port")
	}
}

func TestSyncDiscoversAndPersistsUDPForward(t *testing.T) {
	server := udpDiscoveryServer(t, []map[string]any{
		{"name": "phonebridge", "type": "tcp", "localPort": 8787, "remotePort": 19080, "enabled": true},
		{"name": "udp-video", "type": "udp", "localPort": 3478, "remotePort": 26188, "enabled": true},
	})
	defer server.Close()

	path := filepath.Join(t.TempDir(), "settings.json")
	manager := NewPublicAccessManager(path, "", 8787, nil)
	notified := make(chan struct{}, 1)
	manager.SetUDPForwardListener(func() { notified <- struct{}{} })
	if _, err := manager.Save(context.Background(), PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: server.URL,
		FRPInstanceID: "home",
		FRPTunnelName: "phonebridge",
		FRPScheme:     "https",
		AutoSync:      true,
	}); err != nil {
		t.Fatalf("save with UDP tunnel present: %v", err)
	}
	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("UDP forward listener was not notified after a successful sync")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var saved publicAccessFile
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	if saved.Settings.FRPUDPForward == nil || saved.Settings.FRPUDPForward.PublicPort != 26188 {
		t.Fatalf("UDP mapping not persisted after sync: %#v", saved.Settings.FRPUDPForward)
	}
}
