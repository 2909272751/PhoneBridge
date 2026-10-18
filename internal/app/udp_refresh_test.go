package app

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestAdvertiseUDPForwardReplacesStaleNodeAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var data any
		switch request.URL.Path {
		case "/api/instances":
			data = []map[string]any{{"id": "home", "name": "Home"}}
		case "/api/instances/home/tunnels":
			data = map[string]any{"tunnels": []map[string]any{
				{"name": "phonebridge", "type": "tcp", "localPort": 8787, "remotePort": 19080, "enabled": true},
				{"name": "phonebridge-udp", "type": "udp", "localPort": 3478, "remotePort": 26188, "enabled": true},
			}}
		case "/api/instances/home/config":
			data = map[string]any{"configText": "serverAddr = \"198.51.100.7\"\n"}
		default:
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": data})
	}))
	defer server.Close()

	manager := NewPublicAccessManager(filepath.Join(t.TempDir(), "settings.json"), "", 8787, nil)
	if _, err := manager.Save(context.Background(), PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: server.URL,
		FRPInstanceID: "home",
		FRPTunnelName: "phonebridge",
		FRPScheme:     "http",
	}); err != nil {
		t.Fatalf("seed 88FRP settings: %v", err)
	}

	appServer := &Server{
		config:        Config{ICEPort: 3478},
		logger:        log.New(io.Discard, "", 0),
		publicAccess:  manager,
		icePublicHost: "203.0.113.9",
		icePublicPort: 1111,
	}
	appServer.advertiseUDPForwardLocked(context.Background())
	if appServer.icePublicHost != "198.51.100.7" || appServer.icePublicPort != 26188 {
		t.Fatalf("stale UDP endpoint was not replaced: %#v", appServer)
	}
}
