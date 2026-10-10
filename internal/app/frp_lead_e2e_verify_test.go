package app

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLeadEndToEndSyncReachesFRPReadyWithDesktopMarker is the Lead's independent
// end-to-end check: a mock that reproduces the REAL 88FRP guard extracted from
// 88frp-web.exe (loopback + X-88FRP-Desktop:1, else 401), the real tunnels payload
// shape, and the real config text with serverAddr. It asserts the sync reaches
// frp-ready and derives the exact public URL the user's tunnel implies.
func TestLeadEndToEndSyncReachesFRPReadyWithDesktopMarker(t *testing.T) {
	const instanceID = "0e06bc1f-70ac-436f-a13a-e278f8d4450c"
	const tunnelName = "rdC6YRNXcxc6"
	const serverAddr = "112.74.85.74"
	const remotePort = 24465

	guard := func(w http.ResponseWriter, r *http.Request) bool {
		ip := r.RemoteAddr
		loopback := strings.HasPrefix(ip, "127.0.0.1:") || strings.HasPrefix(ip, "[::1]:")
		if !loopback || r.Header.Get("X-88FRP-Desktop") != "1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"message":"请先登录控制台。","data":null}`))
			return false
		}
		return true
	}

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !guard(w, r) {
			return
		}
		switch {
		case r.URL.Path == "/api/instances":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []map[string]any{{"id": instanceID, "name": "1"}}})
		case r.URL.Path == "/api/instances/"+instanceID+"/tunnels":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"tunnels": []map[string]any{{
				"name": tunnelName, "type": "tcp", "localPort": 8787, "remotePort": remotePort, "enabled": true,
			}}}})
		case r.URL.Path == "/api/instances/"+instanceID+"/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"configText": "serverAddr = \"" + serverAddr + "\"\nserverPort = 1210\n",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	manager := NewPublicAccessManager(settingsPath, "", 8787, log.New(os.Stderr, "", 0))
	snapshot, err := manager.Save(context.Background(), PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: mock.URL,
		FRPInstanceID: instanceID,
		FRPTunnelName: tunnelName,
		FRPScheme:     "http",
		AutoSync:      true,
	})
	if err != nil {
		t.Fatalf("sync must succeed with the desktop marker, got: %v", err)
	}
	if snapshot.State != "frp-ready" {
		t.Fatalf("state = %q (message %q), want frp-ready", snapshot.State, snapshot.Message)
	}
	want := "http://" + serverAddr + ":" + "24465"
	if snapshot.EffectiveURL != want {
		t.Fatalf("effectiveUrl = %q, want %q", snapshot.EffectiveURL, want)
	}
	t.Logf("OK state=%s url=%s", snapshot.State, snapshot.EffectiveURL)
}