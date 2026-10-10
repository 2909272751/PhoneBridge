package app

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newDiagnosticTestServer builds a server whose public path can be driven
// directly, without any 88FRP service or real socket.
func newDiagnosticTestServer(t *testing.T, publicHost string, publicPort int) *Server {
	t.Helper()
	server := New(Config{
		ListenAddress: "127.0.0.1:0",
		SettingsPath:  filepath.Join(t.TempDir(), "settings.json"),
	}, log.New(io.Discard, "", 0))
	server.iceMu.Lock()
	server.icePublicHost = publicHost
	server.icePublicPort = publicPort
	server.iceUDPFresh = true
	server.iceMu.Unlock()
	return server
}

func transportDiagnostics(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload struct {
		WebRTCTransport map[string]any `json:"webrtcTransport"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("status response is not valid JSON: %v", err)
	}
	return payload.WebRTCTransport
}

// /api/status must expose the verified public endpoint and the local mux port,
// and must never leak SDP, candidates, secrets or a device ID.
func TestStatusExposesPublicUDPDiagnostics(t *testing.T) {
	server := newDiagnosticTestServer(t, "112.74.85.74", 26188)
	recorder := httptest.NewRecorder()
	server.handleStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	transport := transportDiagnostics(t, recorder.Body.Bytes())
	if got := transport["udpPublicHost"]; got != "112.74.85.74" {
		t.Fatalf("udpPublicHost = %v, want the mapped public host", got)
	}
	if got := transport["udpPublicPort"]; got != float64(26188) {
		t.Fatalf("udpPublicPort = %v, want 26188", got)
	}
	// No socket is bound here, so the documented default local port applies.
	if got := transport["udpLocalPort"]; got != float64(defaultICEPort) {
		t.Fatalf("udpLocalPort = %v, want %d", got, defaultICEPort)
	}
	if got := transport["udpPortMatched"]; got != true {
		t.Fatalf("udpPortMatched = %v, want true without a persisted mapping", got)
	}

	// The raw body must stay free of connection details.
	for _, forbidden := range []string{"a=candidate", "v=0", "ufrag", "deviceId"} {
		if body := recorder.Body.String(); containsFold(body, forbidden) {
			t.Fatalf("status response leaked %q: %s", forbidden, body)
		}
	}
}

// A persisted mapping whose local port no longer matches the bound mux port is
// the failure class this fix targets: it must be reported as unmatched rather
// than silently presented as a working public path.
func TestStatusReportsUDPPortMismatch(t *testing.T) {
	server := newDiagnosticTestServer(t, "112.74.85.74", 26188)
	// Simulate a mapping discovered for a different local port (e.g. before the
	// mux bound its real socket).
	publicAccess := server.publicAccess.Snapshot()
	settings := publicAccess.Settings
	settings.FRPUDPForward = &UDPForwardMapping{
		PublicHost:   "112.74.85.74",
		PublicPort:   26188,
		LocalICEPort: 40000,
		DiscoveredAt: time.Now(),
	}
	server.publicAccess.setSnapshot(PublicAccessSnapshot{Settings: settings, State: publicAccess.State, Message: publicAccess.Message})

	recorder := httptest.NewRecorder()
	server.handleStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	transport := transportDiagnostics(t, recorder.Body.Bytes())
	if got := transport["udpPortMatched"]; got != false {
		t.Fatalf("udpPortMatched = %v, want false for a 40000 -> %d mismatch", got, defaultICEPort)
	}
}

// The viewer-facing config endpoint backs controller.js's decision to attempt
// realtime media at all, so it must report the same verified public path: an
// unusable (mismatched) mapping must not advertise realtimeExpected.
func TestWebRTCConfigWithdrawsRealtimeOnPortMismatch(t *testing.T) {
	newRequest := func(server *Server) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/api/public/sessions/tok/webrtc/config", nil)
		request.SetPathValue("token", "tok")
		request.Header.Set("X-PhoneBridge-Viewer", "viewer-a")
		recorder := httptest.NewRecorder()
		server.handleWebRTCConfig(recorder, request)
		return recorder
	}

	server := newDiagnosticTestServer(t, "112.74.85.74", 26188)
	server.mu.Lock()
	server.sessions["s1"] = &Session{
		ID: "s1", State: "connected", ViewerState: "connected",
		ViewerToken: "viewer-a", Mode: "control",
	}
	server.sessionToken["tok"] = "s1"
	server.mu.Unlock()

	matched := newRequest(server)
	if matched.Code != http.StatusOK {
		t.Fatalf("config = %d, want 200 (body %s)", matched.Code, matched.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(matched.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["realtimeExpected"] != true || payload["transportHint"] != "frp-udp" {
		t.Fatalf("healthy mapping must advertise realtime: %v", payload)
	}
	if payload["udpPortMatched"] != true {
		t.Fatalf("udpPortMatched = %v, want true", payload["udpPortMatched"])
	}
	if _, err := url.Parse("http://" + server.icePublicHost); err != nil {
		t.Fatal(err)
	}

	// Break the mapping's local port only.
	publicAccess := server.publicAccess.Snapshot()
	settings := publicAccess.Settings
	settings.FRPUDPForward = &UDPForwardMapping{
		PublicHost: "112.74.85.74", PublicPort: 26188, LocalICEPort: 40000, DiscoveredAt: time.Now(),
	}
	server.publicAccess.setSnapshot(PublicAccessSnapshot{Settings: settings})

	mismatched := newRequest(server)
	if err := json.Unmarshal(mismatched.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["udpPortMatched"] != false {
		t.Fatalf("udpPortMatched = %v, want false", payload["udpPortMatched"])
	}
	if payload["realtimeExpected"] != false || payload["udpForwardActive"] != false {
		t.Fatalf("a mismatched mapping must not advertise the realtime path: %v", payload)
	}
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
