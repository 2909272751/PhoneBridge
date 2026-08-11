package app

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// browserProof reproduces exactly what the settings page computes with native
// WebCrypto: PBKDF2-SHA256 derives a key from the password and the
// base64/base64url-decoded challenge salt, then HMAC-SHA256 signs
// `${nonce}\n${username.trim()}` and the result is base64url-encoded (SPEC
// point 3).
func browserProof(t *testing.T, password, username, challenge string, iterations int, nonce string) string {
	t.Helper()
	if iterations <= 0 {
		iterations = 10000
	}
	salt, err := decodeBase64Salt(challenge)
	if err != nil {
		t.Fatalf("decode salt %q: %v", challenge, err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		t.Fatalf("pbkdf2: %v", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(nonce + "\n" + strings.TrimSpace(username)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// mockFRPServer simulates the 88FRP v3 console-auth flow plus the
// authenticated instances API. It verifies the browser-derived proof and
// requires the session cookie for /api/instances.
type mockFRPServer struct {
	server      *httptest.Server
	mu          sync.Mutex
	username    string
	password    string
	challenge   string
	iterations  int
	nonce       string
	challengeID string
	seenProof   string
	gotCookie   bool
	loginCalls  int
}

func newMockFRPServer(t *testing.T, username, password string) *mockFRPServer {
	t.Helper()
	mock := &mockFRPServer{
		username:    username,
		password:    password,
		// Real 88FRP v3 sends the salt base64/base64url-encoded; this mock uses
		// the base64 encoding of "salt-value-42" as the byte salt.
		challenge:   "c2FsdC12YWx1ZS00Mg==",
		iterations:  1000,
		nonce:       "nonce-abc-987",
		challengeID: "challenge-id-1",
	}
	mock.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/console-auth/challenge":
			if request.Method != http.MethodGet {
				http.Error(writer, "challenge 必须使用 GET", http.StatusMethodNotAllowed)
				return
			}
			if request.Body != nil && request.ContentLength > 0 {
				http.Error(writer, "challenge 不允许携带请求体", http.StatusBadRequest)
				return
			}
			// Real 88FRP v3 returns salt (not challenge) on a bare GET.
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"challengeId": mock.challengeID,
					"salt":        mock.challenge,
					"iterations":  mock.iterations,
					"nonce":       mock.nonce,
				},
			})
		case "/api/console-auth/login":
			var body struct {
				Username    string `json:"username"`
				Proof       string `json:"proof"`
				ChallengeID string `json:"challengeId"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			if body.ChallengeID != mock.challengeID {
				_ = json.NewEncoder(writer).Encode(map[string]any{"success": false, "message": "challenge 无效"})
				return
			}
			want := browserProof(t, mock.password, body.Username, mock.challenge, mock.iterations, mock.nonce)
			if body.Username != mock.username || body.Proof != want {
				_ = json.NewEncoder(writer).Encode(map[string]any{"success": false, "message": "用户名或密码不正确"})
				return
			}
			mock.mu.Lock()
			mock.seenProof = body.Proof
			mock.loginCalls++
			mock.mu.Unlock()
			http.SetCookie(writer, &http.Cookie{Name: "frp_session", Value: "token-123", Path: "/"})
			_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": map[string]any{}})
		case "/api/instances":
			mock.mu.Lock()
			cookiePresent := false
			for _, cookie := range request.Cookies() {
				if cookie.Name == "frp_session" && cookie.Value == "token-123" {
					cookiePresent = true
				}
			}
			if cookiePresent {
				mock.gotCookie = true
			}
			mock.mu.Unlock()
			if !cookiePresent {
				http.Error(writer, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": []map[string]any{{"id": "home", "name": "Home"}}})
		case "/api/instances/home/tunnels":
			_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": map[string]any{"tunnels": []map[string]any{}}})
		case "/api/instances/home/config":
			_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": map[string]any{"configText": "serverAddr = \"198.51.100.7\"\n"}})
		default:
			http.NotFound(writer, request)
		}
	}))
	return mock
}

func (mock *mockFRPServer) Close() {
	mock.server.Close()
}

func newRemoteAdminTestService(t *testing.T, mainPort int) (*RemoteAdminService, *PublicAccessManager) {
	t.Helper()
	manager := NewPublicAccessManager(filepath.Join(t.TempDir(), "settings.json"), "", mainPort, nil)
	service := NewRemoteAdminService(manager, log.New(io.Discard, "", 0))
	// Tests run many management actions back to back; disable the 3-second
	// rate limit except in the dedicated rate-limit test.
	service.minActionInterval = 0
	return service, manager
}

// remoteAdminRequest builds a request as seen through the forwarded public
// address: non-loopback peer, same-origin browser request.
func remoteAdminRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, strings.NewReader(body))
	request.RemoteAddr = "203.0.113.5:4444"
	request.Host = "127.0.0.1:8787"
	request.Header.Set("Origin", "http://127.0.0.1:8787")
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestRemoteAdminLoginFlowWithBrowserProof(t *testing.T) {
	mock := newMockFRPServer(t, "alice", "correct-password")
	defer mock.Close()
	service, manager := newRemoteAdminTestService(t, 8787)

	// Challenge: no admin code or bearer is required anymore.
	challengeBody, _ := json.Marshal(map[string]string{"username": "alice", "serviceUrl": mock.server.URL})
	recorder := httptest.NewRecorder()
	service.HandleChallenge(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/challenge", string(challengeBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("challenge status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var challenge struct {
		ChallengeID string `json:"challengeId"`
		Challenge   string `json:"challenge"`
		Iterations  int    `json:"iterations"`
		Nonce       string `json:"nonce"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &challenge); err != nil {
		t.Fatalf("parse challenge: %v", err)
	}
	if challenge.ChallengeID != mock.challengeID || challenge.Challenge != mock.challenge || challenge.Iterations != mock.iterations || challenge.Nonce != mock.nonce {
		t.Fatalf("challenge fields mismatched: %#v", challenge)
	}

	// Login with a browser-derived proof and no password field.
	proof := browserProof(t, "correct-password", "alice", challenge.Challenge, challenge.Iterations, challenge.Nonce)
	loginBody, _ := json.Marshal(map[string]string{"username": "alice", "proof": proof, "challengeId": challenge.ChallengeID})
	recorder = httptest.NewRecorder()
	service.HandleLogin(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/login", string(loginBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Success       bool                 `json:"success"`
		Authenticated bool                 `json:"authenticated"`
		PublicAccess  PublicAccessSnapshot `json:"publicAccess"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("parse login response: %v", err)
	}
	if !payload.Success || !payload.Authenticated {
		t.Fatalf("login not reported successful: %#v", payload)
	}
	if payload.PublicAccess.Settings.Source != publicAccess88FRP {
		t.Fatalf("source was not switched to 88FRP: %#v", payload.PublicAccess.Settings)
	}

	mock.mu.Lock()
	seenProof := mock.seenProof
	gotCookie := mock.gotCookie
	mock.mu.Unlock()
	// The proof forwarded to 88FRP must be the derived proof, never the
	// plaintext password.
	if seenProof == "" || seenProof == "correct-password" {
		t.Fatalf("88FRP received the password itself or no proof: %q", seenProof)
	}
	if want := browserProof(t, "correct-password", "alice", mock.challenge, mock.iterations, mock.nonce); seenProof != want {
		t.Fatalf("proof = %q, want %q", seenProof, want)
	}
	// The in-memory jar must have carried the session cookie into the
	// authenticated /api/instances call made by the automatic sync.
	if !gotCookie {
		t.Fatal("authenticated sync did not use the in-memory 88FRP cookie jar")
	}
	// The login response must never leak the proof or the password.
	responseBody := recorder.Body.String()
	for _, secret := range []string{proof, "correct-password"} {
		if strings.Contains(responseBody, secret) {
			t.Fatalf("login response leaked %q", secret)
		}
	}

	// A second sync through the manager keeps working with the same jar.
	if _, err := manager.Sync(t.Context()); err != nil {
		t.Fatalf("sync after login: %v", err)
	}
}

func TestRemoteAdminLoginRejectsPasswordField(t *testing.T) {
	mock := newMockFRPServer(t, "alice", "correct-password")
	defer mock.Close()
	service, _ := newRemoteAdminTestService(t, 8787)

	// The backend login API must not accept a password field at all: a client
	// that sends one is rejected outright (SPEC: 后端登录 API 不得包含 password 字段).
	body := `{"username":"alice","password":"correct-password","proof":"x","challengeId":"c1"}`
	recorder := httptest.NewRecorder()
	service.HandleLogin(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/login", body))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("login with password field status = %d, want 400", recorder.Code)
	}
}

func TestRemoteAdminChallengeConsumedOnLoginAttempt(t *testing.T) {
	mock := newMockFRPServer(t, "alice", "correct-password")
	defer mock.Close()
	service, _ := newRemoteAdminTestService(t, 8787)

	challengeBody, _ := json.Marshal(map[string]string{"username": "alice", "serviceUrl": mock.server.URL})
	recorder := httptest.NewRecorder()
	service.HandleChallenge(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/challenge", string(challengeBody)))
	var challenge struct {
		ChallengeID string `json:"challengeId"`
		Challenge   string `json:"challenge"`
		Iterations  int    `json:"iterations"`
		Nonce       string `json:"nonce"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &challenge)

	// A wrong proof fails the login…
	wrongBody, _ := json.Marshal(map[string]string{"username": "alice", "proof": "AAAAAAAA", "challengeId": challenge.ChallengeID})
	recorder = httptest.NewRecorder()
	service.HandleLogin(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/login", string(wrongBody)))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-proof login status = %d, want 401", recorder.Code)
	}
	// …and the attempt is consumed: replaying the same challengeId fails.
	goodProof := browserProof(t, "correct-password", "alice", challenge.Challenge, challenge.Iterations, challenge.Nonce)
	replayBody, _ := json.Marshal(map[string]string{"username": "alice", "proof": goodProof, "challengeId": challenge.ChallengeID})
	recorder = httptest.NewRecorder()
	service.HandleLogin(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/login", string(replayBody)))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("replayed challenge status = %d, want 401", recorder.Code)
	}
	if service.Status().Authenticated {
		t.Fatal("failed/replayed login must not leave an authenticated session")
	}
}

func TestRemoteAdminLogoutClearsSession(t *testing.T) {
	mock := newMockFRPServer(t, "alice", "correct-password")
	defer mock.Close()
	service, _ := newRemoteAdminTestService(t, 8787)

	// Log in fully.
	challengeBody, _ := json.Marshal(map[string]string{"username": "alice", "serviceUrl": mock.server.URL})
	recorder := httptest.NewRecorder()
	service.HandleChallenge(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/challenge", string(challengeBody)))
	var challenge struct {
		ChallengeID string `json:"challengeId"`
		Challenge   string `json:"challenge"`
		Iterations  int    `json:"iterations"`
		Nonce       string `json:"nonce"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &challenge)
	proof := browserProof(t, "correct-password", "alice", challenge.Challenge, challenge.Iterations, challenge.Nonce)
	loginBody, _ := json.Marshal(map[string]string{"username": "alice", "proof": proof, "challengeId": challenge.ChallengeID})
	recorder = httptest.NewRecorder()
	service.HandleLogin(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/login", string(loginBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	// Logout clears the session; a fresh challenge still works (no bearer).
	logoutBody, _ := json.Marshal(map[string]string{})
	recorder = httptest.NewRecorder()
	service.HandleLogout(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/logout", string(logoutBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("logout status = %d", recorder.Code)
	}
	if service.Status().Authenticated {
		t.Fatal("session must be cleared after logout")
	}
	recorder = httptest.NewRecorder()
	service.HandleChallenge(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/challenge", string(challengeBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("challenge after logout status = %d, want 200", recorder.Code)
	}
}

func TestRemoteAdminRejectsCrossSiteOrigin(t *testing.T) {
	service, _ := newRemoteAdminTestService(t, 8787)
	body, _ := json.Marshal(map[string]string{"username": "alice"})
	request := remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/challenge", string(body))
	request.Header.Set("Origin", "https://evil.example.test")
	recorder := httptest.NewRecorder()
	service.HandleChallenge(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-site challenge status = %d, want 403", recorder.Code)
	}
}

func TestRemoteAdminRateLimited(t *testing.T) {
	service, _ := newRemoteAdminTestService(t, 8787)
	service.minActionInterval = remoteAdminMinInterval
	body, _ := json.Marshal(map[string]string{"username": "alice"})
	first := remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/challenge", string(body))
	recorder := httptest.NewRecorder()
	service.HandleChallenge(recorder, first)
	second := remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/challenge", string(body))
	recorder = httptest.NewRecorder()
	service.HandleChallenge(recorder, second)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second action status = %d, want 429", recorder.Code)
	}
}

// TestRemoteAdminChallengeLoginNotBlockedByRateLimit verifies the pairing fix:
// with the real 3-second interval re-enabled, a normal challenge→login runs
// immediately (no waiting on the shared slot), consecutive challenges still
// return 429, and a consumed attempt cannot be replayed (SPEC 要求 2/3/4).
func TestRemoteAdminChallengeLoginNotBlockedByRateLimit(t *testing.T) {
	mock := newMockFRPServer(t, "alice", "correct-password")
	defer mock.Close()
	service, _ := newRemoteAdminTestService(t, 8787)
	// Re-enable the real 3-second interval so the test proves a normal login is
	// not blocked by the challenge slot.
	service.minActionInterval = remoteAdminMinInterval

	challengeBody, _ := json.Marshal(map[string]string{"username": "alice", "serviceUrl": mock.server.URL})
	recorder := httptest.NewRecorder()
	service.HandleChallenge(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/challenge", string(challengeBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("challenge status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var challenge struct {
		ChallengeID string `json:"challengeId"`
		Challenge   string `json:"challenge"`
		Iterations  int    `json:"iterations"`
		Nonce       string `json:"nonce"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &challenge); err != nil {
		t.Fatalf("parse challenge: %v", err)
	}

	// A matching, unexpired, one-shot attempt lets login run immediately —
	// without waiting out the challenge interval.
	proof := browserProof(t, "correct-password", "alice", challenge.Challenge, challenge.Iterations, challenge.Nonce)
	loginBody, _ := json.Marshal(map[string]string{"username": "alice", "proof": proof, "challengeId": challenge.ChallengeID})
	recorder = httptest.NewRecorder()
	service.HandleLogin(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/login", string(loginBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("login right after challenge status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// Consecutive challenges remain rate-limited.
	recorder = httptest.NewRecorder()
	service.HandleChallenge(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/challenge", string(challengeBody)))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("consecutive challenge status = %d, want 429", recorder.Code)
	}

	// The consumed attempt cannot be replayed.
	recorder = httptest.NewRecorder()
	service.HandleLogin(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/login", string(loginBody)))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("replayed login status = %d, want 401", recorder.Code)
	}
}

func TestRemoteAdminRejectsNonJSONContentType(t *testing.T) {
	service, _ := newRemoteAdminTestService(t, 8787)
	request := remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/challenge", "username=alice")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	service.HandleChallenge(recorder, request)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON status = %d, want 415", recorder.Code)
	}
}

func TestRemoteAdminStatusHasNoSecrets(t *testing.T) {
	service, _ := newRemoteAdminTestService(t, 8787)
	recorder := httptest.NewRecorder()
	service.HandleStatus(recorder, remoteAdminRequest(t, http.MethodGet, "/api/remote-admin/status", ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, secret := range []string{"password", "proof", "cookie", "username", "bearer", "admin"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(secret)) {
			t.Fatalf("remote-admin status leaked %q", secret)
		}
	}
}

func TestServerStatusReportsRemoteAdminWithoutSecrets(t *testing.T) {
	server := New(Config{
		ListenAddress: "127.0.0.1:8787",
		SettingsPath:  filepath.Join(t.TempDir(), "settings.json"),
	}, log.New(io.Discard, "", 0))
	if server.localAdmin == nil {
		t.Fatal("Server must create the remote admin service")
	}
	request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	server.handleStatus(recorder, request)
	var payload struct {
		LocalAdmin LocalAdminStatus `json:"localAdmin"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("parse status: %v", err)
	}
	if payload.LocalAdmin.Authenticated {
		t.Fatal("localAdmin must report not authenticated before login")
	}
	body := recorder.Body.String()
	for _, secret := range []string{"password", "proof", "cookie", "username", "bearer", "adminCode"} {
		if strings.Contains(strings.ToLower(body), secret) {
			t.Fatalf("/api/status must not carry login secrets, found %q", secret)
		}
	}
}

func TestRemoteAdminLoginSyncsInstancesAndPersistsMapping(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/console-auth/challenge":
			if request.Method != http.MethodGet || (request.Body != nil && request.ContentLength > 0) {
				http.Error(writer, "challenge 必须是无请求体 GET", http.StatusMethodNotAllowed)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"challengeId": "cid", "salt": "salt", "iterations": 100, "nonce": "n-1"},
			})
		case "/api/console-auth/login":
			http.SetCookie(writer, &http.Cookie{Name: "frp_session", Value: "token-123", Path: "/"})
			_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": map[string]any{}})
		case "/api/instances":
			cookieOK := false
			for _, cookie := range request.Cookies() {
				if cookie.Name == "frp_session" {
					cookieOK = true
				}
			}
			if !cookieOK {
				http.Error(writer, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": []map[string]any{{"id": "home", "name": "Home"}}})
		case "/api/instances/home/tunnels":
			_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": map[string]any{"tunnels": []map[string]any{
				{"name": "phonebridge", "type": "tcp", "localPort": 8787, "remotePort": 19080, "enabled": true},
				{"name": "udp-video", "type": "udp", "localPort": 3478, "remotePort": 26188, "enabled": true},
			}}})
		case "/api/instances/home/config":
			_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": map[string]any{"configText": "serverAddr = \"198.51.100.7\"\n"}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer mockServer.Close()

	manager := NewPublicAccessManager(filepath.Join(t.TempDir(), "settings.json"), "", 8787, nil)
	notified := make(chan struct{}, 1)
	manager.SetUDPForwardListener(func() { notified <- struct{}{} })
	service := NewRemoteAdminService(manager, log.New(io.Discard, "", 0))
	service.minActionInterval = 0

	// First login: no instance selected yet, so the UI must choose (SPEC: 多个
	// 候选无法唯一确定时在 UI 选择).
	challengeBody, _ := json.Marshal(map[string]string{"username": "alice", "serviceUrl": mockServer.URL})
	recorder := httptest.NewRecorder()
	service.HandleChallenge(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/challenge", string(challengeBody)))
	var challenge struct {
		ChallengeID string `json:"challengeId"`
		Challenge   string `json:"challenge"`
		Iterations  int    `json:"iterations"`
		Nonce       string `json:"nonce"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &challenge)
	proof := browserProof(t, "correct-password", "alice", challenge.Challenge, challenge.Iterations, challenge.Nonce)
	loginBody, _ := json.Marshal(map[string]string{"username": "alice", "proof": proof, "challengeId": challenge.ChallengeID})
	recorder = httptest.NewRecorder()
	service.HandleLogin(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/login", string(loginBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		PublicAccess PublicAccessSnapshot `json:"publicAccess"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &payload)
	if payload.PublicAccess.State != "frp-needs-instance" {
		t.Fatalf("first login state = %q, want frp-needs-instance (UI choice)", payload.PublicAccess.State)
	}
	if len(payload.PublicAccess.Instances) != 1 || payload.PublicAccess.Instances[0].ID != "home" {
		t.Fatalf("instances not read for UI choice: %#v", payload.PublicAccess.Instances)
	}

	// The user picks the instance and tunnel, then saves through the public
	// settings API; the full sync persists TCP + UDP mappings and refreshes ICE.
	settings := payload.PublicAccess.Settings
	settings.FRPInstanceID = "home"
	settings.FRPTunnelName = "phonebridge"
	settings.FRPScheme = "https"
	snapshot, err := manager.Save(t.Context(), settings)
	if err != nil {
		t.Fatalf("save after selection: %v", err)
	}
	if snapshot.State != "frp-ready" || snapshot.EffectiveURL != "https://198.51.100.7:19080" {
		t.Fatalf("unexpected final snapshot: %#v", snapshot)
	}
	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("UDP forward listener was not notified after the synced login")
	}
}

func TestPublicSettingsWriteAllowsRemoteWithoutBearer(t *testing.T) {
	server := New(Config{
		ListenAddress: "127.0.0.1:8787",
		SettingsPath:  filepath.Join(t.TempDir(), "settings.json"),
	}, log.New(io.Discard, "", 0))
	handler, err := server.routes()
	if err != nil {
		t.Fatalf("routes: %v", err)
	}

	// A remote (non-loopback) settings write succeeds without a bearer: the
	// user chose a single-user setup with no admin code (SPEC: 公网设置 PUT 与
	// 手动 Sync 不再要求 bearer).
	request := httptest.NewRequest(http.MethodPut, "/api/public-access", strings.NewReader(`{"source":"manual","manualUrl":"https://share.example.test"}`))
	request.RemoteAddr = "203.0.113.5:4444"
	request.Host = "127.0.0.1:8787"
	request.Header.Set("Origin", "http://127.0.0.1:8787")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("remote settings write without bearer status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// The loopback page keeps working the same way.
	request = httptest.NewRequest(http.MethodPut, "/api/public-access", strings.NewReader(`{"source":"manual","manualUrl":"https://share.example.test"}`))
	request.RemoteAddr = "127.0.0.1:55555"
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("loopback settings write status = %d, want 200", recorder.Code)
	}
}

// --- credential persistence and automatic sign-in ---

// localAdminRequest builds a request as seen on the local machine: loopback
// peer, loopback Host and a matching loopback Origin.
func localAdminRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:55555"
	request.Host = "127.0.0.1:8787"
	request.Header.Set("Origin", "http://127.0.0.1:8787")
	request.Header.Set("Content-Type", "application/json")
	return request
}

// requireCredentialStoreAvailable skips tests that depend on the platform
// credential store (non-Windows platforms cannot persist credentials).
func requireCredentialStoreAvailable(t *testing.T, service *RemoteAdminService) {
	t.Helper()
	if !credentialStoreSupported() || service.store == nil {
		t.Skip("credential store is unsupported on this platform")
	}
}

// TestSaveCredentialsRequiresLoopbackAndLocalOrigin verifies the save
// endpoint accepts only genuine local-machine requests: non-loopback peers,
// forged forwarded headers, public Hosts and cross-site Origins are all
// rejected (SPEC point 2).
func TestSaveCredentialsRequiresLoopbackAndLocalOrigin(t *testing.T) {
	service, _ := newRemoteAdminTestService(t, 8787)
	body := `{"serviceUrl":"http://127.0.0.1:8801","username":"alice","password":"pw","remember":true}`

	// A non-loopback peer is rejected.
	recorder := httptest.NewRecorder()
	service.HandleSaveCredentials(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/credentials", body))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote peer status = %d, want 403", recorder.Code)
	}

	// A loopback peer carrying a forged forwarded header is rejected.
	request := localAdminRequest(t, http.MethodPost, "/api/remote-admin/credentials", body)
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	recorder = httptest.NewRecorder()
	service.HandleSaveCredentials(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("forwarded header status = %d, want 403", recorder.Code)
	}

	// A loopback peer with a public Host (as seen through the 88FRP tunnel) is
	// rejected even though it lands on 127.0.0.1.
	request = localAdminRequest(t, http.MethodPost, "/api/remote-admin/credentials", body)
	request.Host = "frp.example.test"
	request.Header.Set("Origin", "https://frp.example.test")
	recorder = httptest.NewRecorder()
	service.HandleSaveCredentials(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("public host status = %d, want 403", recorder.Code)
	}

	// A loopback peer with a cross-site Origin is rejected.
	request = localAdminRequest(t, http.MethodPost, "/api/remote-admin/credentials", body)
	request.Header.Set("Origin", "https://evil.example.test")
	recorder = httptest.NewRecorder()
	service.HandleSaveCredentials(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-site origin status = %d, want 403", recorder.Code)
	}

	// A genuine local request is accepted.
	recorder = httptest.NewRecorder()
	service.HandleSaveCredentials(recorder, localAdminRequest(t, http.MethodPost, "/api/remote-admin/credentials", body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("local save status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// TestSaveCredentialsResponseHasNoSensitiveEcho verifies the save endpoint
// never echoes username, password or file material (SPEC points 2 and 8).
func TestSaveCredentialsResponseHasNoSensitiveEcho(t *testing.T) {
	service, _ := newRemoteAdminTestService(t, 8787)
	body := `{"serviceUrl":"http://127.0.0.1:8801","username":"alice","password":"super-secret-pw","remember":true}`
	recorder := httptest.NewRecorder()
	service.HandleSaveCredentials(recorder, localAdminRequest(t, http.MethodPost, "/api/remote-admin/credentials", body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("save status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	for _, secret := range []string{"alice", "super-secret-pw", "frp-credentials", "8801"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("save response leaked %q", secret)
		}
	}
}

// TestSaveCredentialsRememberFalseClears verifies that unchecking "remember
// login" clears any previously saved credentials (SPEC point 3).
func TestSaveCredentialsRememberFalseClears(t *testing.T) {
	service, _ := newRemoteAdminTestService(t, 8787)
	requireCredentialStoreAvailable(t, service)
	recorder := httptest.NewRecorder()
	service.HandleSaveCredentials(recorder, localAdminRequest(t, http.MethodPost, "/api/remote-admin/credentials", `{"serviceUrl":"http://127.0.0.1:8801","username":"alice","password":"pw","remember":true}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("save status = %d", recorder.Code)
	}
	if !service.store.HasSaved() {
		t.Fatal("credentials should be saved")
	}
	recorder = httptest.NewRecorder()
	service.HandleSaveCredentials(recorder, localAdminRequest(t, http.MethodPost, "/api/remote-admin/credentials", `{"remember":false}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("clear status = %d", recorder.Code)
	}
	if service.store.HasSaved() {
		t.Fatal("credentials must be cleared when remember is unchecked")
	}
}

// TestLogoutOnLocalMachineClearsSavedCredentials verifies that logging out
// from the local page also clears the saved credentials so a restart does not
// sign back in (SPEC point 6).
func TestLogoutOnLocalMachineClearsSavedCredentials(t *testing.T) {
	service, _ := newRemoteAdminTestService(t, 8787)
	requireCredentialStoreAvailable(t, service)
	recorder := httptest.NewRecorder()
	service.HandleSaveCredentials(recorder, localAdminRequest(t, http.MethodPost, "/api/remote-admin/credentials", `{"serviceUrl":"http://127.0.0.1:8801","username":"alice","password":"pw","remember":true}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("save status = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	service.HandleLogout(recorder, localAdminRequest(t, http.MethodPost, "/api/remote-admin/logout", `{}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("logout status = %d", recorder.Code)
	}
	if service.store.HasSaved() {
		t.Fatal("local logout must clear saved credentials")
	}
}

// TestLogoutFromPublicPageKeepsSavedCredentials verifies a public-page logout
// only ends the in-memory session and cannot touch the local credentials.
func TestLogoutFromPublicPageKeepsSavedCredentials(t *testing.T) {
	service, _ := newRemoteAdminTestService(t, 8787)
	requireCredentialStoreAvailable(t, service)
	recorder := httptest.NewRecorder()
	service.HandleSaveCredentials(recorder, localAdminRequest(t, http.MethodPost, "/api/remote-admin/credentials", `{"serviceUrl":"http://127.0.0.1:8801","username":"alice","password":"pw","remember":true}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("save status = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	service.HandleLogout(recorder, remoteAdminRequest(t, http.MethodPost, "/api/remote-admin/logout", `{}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("logout status = %d", recorder.Code)
	}
	if !service.store.HasSaved() {
		t.Fatal("public logout must not clear saved credentials")
	}
}

// TestAutoLoginRestoresSavedSession runs the full automatic sign-in against a
// mock 88FRP server: challenge, server-side proof, login and the automatic
// Save+Sync with the authenticated cookie jar (SPEC point 4).
func TestAutoLoginRestoresSavedSession(t *testing.T) {
	mock := newMockFRPServer(t, "alice", "correct-password")
	defer mock.Close()
	service, manager := newRemoteAdminTestService(t, 8787)
	requireCredentialStoreAvailable(t, service)
	if err := service.store.Save(storedCredentials{
		ServiceURL: mock.server.URL,
		Username:   "alice",
		Password:   "correct-password",
		Remember:   true,
	}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	if err := service.autoLoginOnce(context.Background()); err != nil {
		t.Fatalf("auto login: %v", err)
	}
	status := service.Status()
	if !status.Authenticated {
		t.Fatal("auto login must authenticate")
	}
	if status.AutoLoginState != "done" {
		t.Fatalf("auto login state = %q, want done", status.AutoLoginState)
	}
	if !status.Saved {
		t.Fatal("credentials must still be saved after auto login")
	}
	snapshot := manager.Snapshot()
	if snapshot.Settings.Source != publicAccess88FRP {
		t.Fatalf("source not switched to 88FRP: %q", snapshot.Settings.Source)
	}
	mock.mu.Lock()
	gotCookie := mock.gotCookie
	mock.mu.Unlock()
	if !gotCookie {
		t.Fatal("auto-login sync did not use the authenticated cookie jar")
	}
}

// TestAutoLoginFailureDoesNotBlock verifies a wrong saved password fails the
// auto-login, leaves the service unauthenticated yet fully responsive, and
// reports the failed state through the status API (SPEC point 4).
func TestAutoLoginFailureDoesNotBlock(t *testing.T) {
	mock := newMockFRPServer(t, "alice", "correct-password")
	defer mock.Close()
	service, _ := newRemoteAdminTestService(t, 8787)
	requireCredentialStoreAvailable(t, service)
	if err := service.store.Save(storedCredentials{
		ServiceURL: mock.server.URL,
		Username:   "alice",
		Password:   "wrong-password",
		Remember:   true,
	}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	if err := service.autoLoginOnce(context.Background()); err == nil {
		t.Fatal("auto login with a wrong password must fail")
	}
	status := service.Status()
	if status.Authenticated {
		t.Fatal("failed auto login must not authenticate")
	}
	if status.AutoLoginState != "failed" {
		t.Fatalf("auto login state = %q, want failed", status.AutoLoginState)
	}
	recorder := httptest.NewRecorder()
	service.HandleStatus(recorder, remoteAdminRequest(t, http.MethodGet, "/api/remote-admin/status", ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status after failed auto login = %d", recorder.Code)
	}
}

// TestAutoLoginWithoutCredentialsIsIdle verifies that with no saved
// credentials the auto-login is a silent no-op (idle), not an error.
func TestAutoLoginWithoutCredentialsIsIdle(t *testing.T) {
	service, _ := newRemoteAdminTestService(t, 8787)
	if err := service.autoLoginOnce(context.Background()); err != nil {
		t.Fatalf("auto login without credentials: %v", err)
	}
	if status := service.Status(); status.AutoLoginState != "idle" {
		t.Fatalf("auto login state = %q, want idle", status.AutoLoginState)
	}
}

// TestDeriveFRPProofServerMatchesBrowserProof verifies the backend proof
// derivation is byte-for-byte identical to the browser algorithm (SPEC point
// 5): PBKDF2-SHA256 32-byte key, HMAC over `${nonce}\n${username.trim()}`
// base64url-encoded.
func TestDeriveFRPProofServerMatchesBrowserProof(t *testing.T) {
	// challenges are base64/base64url encodings of the raw byte salts:
	// "c2FsdC12YWx1ZS00Mg=="  -> "salt-value-42" (standard base64)
	// "5a-G56CB8J-UkA"        -> "密码🔐"          (base64url, unicode bytes)
	// "cmFuZG9tLWNoYWxsZW5nZQ==" -> "random-challenge" (standard base64)
	// "c2FsdA=="              -> "salt"           (standard base64)
	vectors := []struct {
		password, username, challenge string
		iterations                    int
		nonce                         string
	}{
		{"correct-password", "alice", "c2FsdC12YWx1ZS00Mg==", 1000, "nonce-abc-987"},
		{"密码🔐", "用户 名", "5a-G56CB8J-UkA", 1, "n-1"},
		{"p@ss w0rd!", "bob-01", "cmFuZG9tLWNoYWxsZW5nZQ==", 4096, "n-2"},
		{"password", "alice", "c2FsdA==", 0, "nonce"}, // iterations<=0 falls back to the default
	}
	for _, vector := range vectors {
		got, err := deriveFRPProofServer(vector.password, vector.username, vector.challenge, vector.iterations, vector.nonce)
		if err != nil {
			t.Fatalf("derive proof for %#v: %v", vector, err)
		}
		want := browserProof(t, vector.password, vector.username, vector.challenge, vector.iterations, vector.nonce)
		if got != want {
			t.Fatalf("proof mismatch for %#v: got %q want %q", vector, got, want)
		}
	}
}

// TestDecodeBase64Salt verifies the challenge-salt decoder accepts standard
// and URL-safe base64 with and without padding, trims surrounding whitespace,
// and rejects undecodable salts with an error instead of falling back to UTF-8
// (SPEC points 1-4).
func TestDecodeBase64Salt(t *testing.T) {
	vectors := []struct {
		encoded string
		want    string
	}{
		{"aGVsbG8=", "hello"},                                            // standard base64 with padding
		{"aGVsbG8", "hello"},                                             // raw standard base64, no padding
		{"-__-AAEC", string([]byte{0xfb, 0xff, 0xfe, 0x00, 0x01, 0x02})}, // base64url, non-ASCII bytes
		{"c2FsdC3nm5DlgLw", "salt-盐值"},                                // base64url, unicode bytes
		{" c2FsdA== ", "salt"},                                          // surrounding whitespace trimmed
	}
	for _, vector := range vectors {
		got, err := decodeBase64Salt(vector.encoded)
		if err != nil {
			t.Fatalf("decode %q: %v", vector.encoded, err)
		}
		if string(got) != vector.want {
			t.Fatalf("decode %q = %q, want %q", vector.encoded, got, vector.want)
		}
	}
	// Undecodable salts must return an error, never a silent UTF-8 fallback.
	for _, bad := range []string{"not-base64!!!", "invalid*salt", "!!!!", "a b c="} {
		if _, err := decodeBase64Salt(bad); err == nil {
			t.Fatalf("decode %q should return an error, got nil", bad)
		}
	}
}

// TestRemoteAdminStatusReportsSavedStateWithoutSecrets verifies the status
// payload carries only the saved/auto-login booleans and enum, never
// username, password, file path or ciphertext (SPEC point 7).
func TestRemoteAdminStatusReportsSavedStateWithoutSecrets(t *testing.T) {
	service, _ := newRemoteAdminTestService(t, 8787)
	recorder := httptest.NewRecorder()
	service.HandleStatus(recorder, remoteAdminRequest(t, http.MethodGet, "/api/remote-admin/status", ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"saved"`) || !strings.Contains(body, `"autoLoginState"`) {
		t.Fatalf("status must report saved/autoLoginState, got %s", body)
	}
	for _, secret := range []string{"password", "proof", "cookie", "username", "ciphertext", "frp-credentials"} {
		if strings.Contains(strings.ToLower(body), secret) {
			t.Fatalf("remote-admin status leaked %q", secret)
		}
	}
}
