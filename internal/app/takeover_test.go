package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestViewerRefreshReusesLeaseAndTakeoverReturnsStructuredConflict(t *testing.T) {
	now := time.Now()
	session, err := newSession(CreateSessionRequest{}, Device{ID: "demo", Model: "Demo", IsDemo: true}, "http://127.0.0.1:8787", now)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		config:           Config{},
		logger:           log.New(io.Discard, "", 0),
		sessions:         map[string]*Session{session.ID: &session},
		sessionToken:     map[string]string{session.Token: session.ID},
		activeID:         session.ID,
		webrtcPeers:      map[string]*webRTCPeer{},
		sessionStorePath: filepath.Join(t.TempDir(), "sessions.json"),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/public/sessions/{token}/join", server.handleJoinSession)
	mux.HandleFunc("GET /api/public/sessions/{token}", server.handlePublicSession)
	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()

	join := func(viewerToken string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"code": "", "viewerToken": viewerToken})
		request, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/api/public/sessions/"+session.Token+"/join", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/140.0 Safari/537.36")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(response.Body)
			t.Fatalf("join status=%d body=%s", response.StatusCode, raw)
		}
		var payload map[string]any
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}

	first := join("")
	firstToken, _ := first["viewerToken"].(string)
	if firstToken == "" {
		t.Fatal("first join did not return a viewer token")
	}
	refreshed := join(firstToken)
	if refreshed["viewerToken"] != firstToken {
		t.Fatalf("refresh should reuse the lease token: got %v want %v", refreshed["viewerToken"], firstToken)
	}
	if refreshed["controllerLabel"] != "Chrome · Windows" {
		t.Fatalf("unexpected controller label: %v", refreshed["controllerLabel"])
	}

	second := join("")
	secondToken, _ := second["viewerToken"].(string)
	if secondToken == "" || secondToken == firstToken {
		t.Fatalf("a new viewer should receive a different token: %q", secondToken)
	}

	request, _ := http.NewRequest(http.MethodGet, httpServer.URL+"/api/public/sessions/"+session.Token, nil)
	request.Header.Set("X-PhoneBridge-Viewer", firstToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale viewer status=%d want 409", response.StatusCode)
	}
	var conflict map[string]any
	if err := json.NewDecoder(response.Body).Decode(&conflict); err != nil {
		t.Fatal(err)
	}
	if conflict["code"] != "control_taken_over" || conflict["canReclaim"] != true {
		t.Fatalf("unexpected conflict payload: %#v", conflict)
	}
}
