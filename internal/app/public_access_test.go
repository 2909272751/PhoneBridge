package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
