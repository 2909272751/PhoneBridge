package app

import (
	"net/http"
	"strings"
	"testing"
)

// TestStalePeerCannotOverwriteSessionState is the generation guard at the
// heart of the reconnect reliability fix. When the server replaces a peer
// (a fresh WebRTC offer, or a new viewer joining), late asynchronous callbacks
// from the replaced peer — connection-state changes, scrcpy stream failures —
// must never overwrite the newer peer's session state with
// fallback-screen/reconnecting.
func TestStalePeerCannotOverwriteSessionState(t *testing.T) {
	server := &Server{
		sessions: map[string]*Session{
			"s1": {ID: "s1", State: "connected", ViewerState: "connected", ViewerToken: "viewer-a", ConnectionMode: "webrtc"},
		},
		sessionToken: map[string]string{"tok": "s1"},
		webrtcPeers:  map[string]*webRTCPeer{},
		activeID:     "s1",
		// An empty sessionStorePath makes persistSessionsLocked a no-op.
	}
	oldPeer := &webRTCPeer{server: server, sessionID: "s1"}
	newPeer := &webRTCPeer{server: server, sessionID: "s1"}
	server.webrtcPeers["s1"] = newPeer

	// A stale peer's asynchronous callback tries to mark the session as
	// reconnecting; it must be ignored because it is no longer registered.
	oldPeer.setSessionState("reconnecting")
	if got := server.sessions["s1"].ConnectionMode; got != "webrtc" {
		t.Fatalf("stale peer overwrote session state: got %q, want %q", got, "webrtc")
	}

	// The currently registered peer may still write session state.
	newPeer.setSessionState("reconnecting")
	if got := server.sessions["s1"].ConnectionMode; got != "reconnecting" {
		t.Fatalf("current peer could not write session state: got %q, want %q", got, "reconnecting")
	}
}

// TestOfferConflictStatus pins the takeover semantics for a WebRTC offer that
// finishes after the viewer lease changed hands. A stale viewer must receive
// 409 (takeover) so the browser stops retrying instead of looping forever;
// an ended session must receive 410.
func TestOfferConflictStatus(t *testing.T) {
	connected := &Session{State: "connected", ViewerState: "connected", ViewerToken: "viewer-a"}
	if status, message := offerConflictStatus(connected, "viewer-a"); status != 0 || message != "" {
		t.Fatalf("matching viewer: got (%d, %q), want (0, \"\")", status, message)
	}
	if status, _ := offerConflictStatus(connected, "viewer-b"); status != http.StatusConflict {
		t.Fatalf("stale viewer: got status %d, want %d", status, http.StatusConflict)
	}
	if status, _ := offerConflictStatus(&Session{State: "expired", ViewerState: "disconnected", ViewerToken: "viewer-a"}, "viewer-a"); status != http.StatusGone {
		t.Fatalf("expired session: got status %d, want %d", status, http.StatusGone)
	}
	if status, _ := offerConflictStatus(&Session{State: "stopped", ViewerState: "disconnected", ViewerToken: "viewer-a"}, "viewer-a"); status != http.StatusGone {
		t.Fatalf("stopped session: got status %d, want %d", status, http.StatusGone)
	}
	if status, _ := offerConflictStatus(&Session{State: "connected", ViewerState: "waiting", ViewerToken: "viewer-a"}, "viewer-a"); status != http.StatusGone {
		t.Fatalf("not-connected session: got status %d, want %d", status, http.StatusGone)
	}
	if status, _ := offerConflictStatus(nil, "viewer-a"); status != http.StatusGone {
		t.Fatalf("nil session: got status %d, want %d", status, http.StatusGone)
	}
}

func TestControllerReconnectStateMachineInvariants(t *testing.T) {
	source, err := embeddedWeb.ReadFile("web/controller.js")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if strings.Contains(text, "reconnectAttempts >= 3") {
		t.Fatal("controller still gives up permanently after three reconnect attempts")
	}
	if !strings.Contains(text, `peerConnection.connectionState !== "connected"`) {
		t.Fatal("media recovery does not require a connected peer connection")
	}
	if !strings.Contains(text, `connection.connectionState === "connected"`) {
		t.Fatal("controller does not clear reconnect state when the peer connection recovers")
	}
	pollStart := strings.Index(text, "async function pollSession()")
	if pollStart < 0 {
		t.Fatal("unable to locate pollSession for reconnect regression check")
	}
	pollEnd := strings.Index(text[pollStart:], "async function sendControl")
	if pollEnd < 0 {
		t.Fatal("unable to locate pollSession for reconnect regression check")
	}
	pollBody := text[pollStart : pollStart+pollEnd]
	if !strings.Contains(pollBody, "hideReconnect()") {
		t.Fatal("HTTP recovery does not re-evaluate the guarded reconnect state")
	}
	hideStart := strings.Index(text, "function hideReconnect()")
	if hideStart < 0 {
		t.Fatal("unable to locate guarded hideReconnect")
	}
	hideEnd := strings.Index(text[hideStart:], "function startFallbackFrames()")
	if hideEnd < 0 || !strings.Contains(text[hideStart:hideStart+hideEnd], "mediaRestored()") {
		t.Fatal("hideReconnect must remain guarded by real media recovery")
	}
	if !strings.Contains(text[hideStart:hideStart+hideEnd], "WebRTC 已连接") {
		t.Fatal("recovered connection does not restore the visible status chip")
	}
}
