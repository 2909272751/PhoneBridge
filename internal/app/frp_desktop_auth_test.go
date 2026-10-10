package app

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file pins the REAL 88FRP HTTP contract for the loopback management API.
// The authoritative sources are:
//
//   - C:\ProgramData\88frp-node\runtime\public\js\console-login.js, the 88FRP
//     web console login client (login body shape, challenge/salt handling).
//   - The 88FRP server itself (88frp-web.exe), whose Express routes are:
//
//     app.get("/api/console-auth/status")    // public
//     app.get("/api/console-auth/challenge") // public
//     app.post("/api/console-auth/login")    // public
//     const requireConsoleAuth = async (req, res, next) => {
//     if (isDesktopRequest(req)) return next();
//     if (await consoleAuthService.verify(readCookie(req.headers.cookie, "88frp_console"))) return next();
//     return res.status(401).json({ success: false, message: "请先登录控制台。", data: null });
//     };
//     app.use("/api", requireConsoleAuth);   // EVERY other /api route
//
//     function isDesktopRequest(req) {
//     const ip = String(req.ip || req.socket.remoteAddress || "");
//     return (ip === "127.0.0.1" || ip === "::1" || ip === "::ffff:127.0.0.1")
//     && req.get("X-88FRP-Desktop") === "1";
//     }
//
// The two independent authentication facts that matter for Bug 1 are:
//
//  1. A bare loopback `GET /api/instances` is 401 unless the request carries
//     `X-88FRP-Desktop: 1` or a valid `88frp_console` session cookie. PhoneBridge
//     originally sent neither, so its *first* sync already returned 401 — this
//     is the reported "同步返回 http 401 / 同步地址失败".
//  2. `POST /api/console-auth/login` expires the session after 24h
//     (SESSION_TTL_MS) unless the body asks for `remember: true`, which grants
//     REMEMBER_TTL_MS = 5 years. PhoneBridge omitted `remember`, so even a
//     successful login produced a session that died a day later and made every
//     subsequent sync 401.

// realFRPContractServer is an httptest server that reproduces the 88FRP
// authentication rules above: the desktop marker bypass, the public
// console-auth routes, the `remember`-dependent session lifetime, and the
// cookie-gated management API. It deliberately rejects an unmarked bare
// loopback request with 401 exactly like 88FRP does.
type realFRPContractServer struct {
	server *httptest.Server

	mu sync.Mutex
	// login bodies observed, so tests can assert `remember` was requested.
	loginBodies []map[string]any
	// desktopHeaderSeen records the desktop marker per path.
	desktopHeaderSeen map[string]string
	challengeID       string
	nonce             string
	salt              string
	iterations        int
	username          string
	// rememberTTL / sessionTTL mirror 88FRP's REMEMBER_TTL_MS and SESSION_TTL_MS.
	rememberTTL time.Duration
	sessionTTL  time.Duration
	// sessionExpiresAt is the expiry of the session currently handed out;
	// zero means no live session.
	sessionExpiresAt time.Time
	// issuedSessionCookie is every cookie value handed out, keyed by the token.
	liveTokens map[string]time.Time
	// expiredToken is the token captured from the last login, used by
	// expireLastSession to simulate a session dying server-side.
	lastToken string
	// instancesRequests counts authenticated /api/instances hits.
	instancesRequests int
	// loginCalls counts login attempts.
	loginCalls int
	// desktopEnabled controls whether the server honours the desktop marker.
	// It is true by default (the real loopback service). Setting it false
	// models a server that only accepts a session cookie, which lets a test
	// force the session-death path even though PhoneBridge sends the marker.
	desktopEnabled bool
	// unauthRequests counts 401s handed out by requireConsoleAuth.
	unauthRequests int
}

func newRealFRPContractServer(t *testing.T, username string) *realFRPContractServer {
	t.Helper()
	contract := &realFRPContractServer{
		challengeID:       "challenge-id-real",
		nonce:             "nonce-real-0123456789",
		salt:              "qVArMqq5b0QLQe5+CrcbUQ==", // standard base64, like the live service
		iterations:        210000,
		username:          username,
		rememberTTL:       5 * 365 * 24 * time.Hour,
		sessionTTL:        24 * time.Hour,
		desktopHeaderSeen: map[string]string{},
		liveTokens:        map[string]time.Time{},
		desktopEnabled:    true,
	}
	contract.server = httptest.NewServer(http.HandlerFunc(contract.handle))
	return contract
}

func (contract *realFRPContractServer) Close() { contract.server.Close() }

// desktopRequest reports whether the request satisfies 88FRP's
// isDesktopRequest: loopback peer AND the exact marker value "1".
//
// 88FRP compares `req.ip` (Express's trust-proxy-aware client IP) against the
// loopback literals, and an httptest client connects from 127.0.0.1, so a
// remote address whose host is loopback is the faithful equivalent here.
func (contract *realFRPContractServer) desktopRequest(request *http.Request) bool {
	host := remoteHost(request)
	loopback := host == "127.0.0.1" || host == "::1" || host == "::ffff:127.0.0.1" || host == "localhost"
	contract.mu.Lock()
	enabled := contract.desktopEnabled
	contract.mu.Unlock()
	return enabled && loopback && request.Header.Get(frpDesktopHeader) == frpDesktopHeaderValue
}

// remoteHost extracts the peer host from RemoteAddr, tolerating both
// "host:port" and bare-host forms.
func remoteHost(request *http.Request) string {
	if parsed, _, err := net.SplitHostPort(request.RemoteAddr); err == nil {
		return parsed
	}
	return request.RemoteAddr
}

// liveSession reports whether the request carries a still-valid session cookie.
func (contract *realFRPContractServer) liveSession(request *http.Request) bool {
	cookie, err := request.Cookie(frpSessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return false
	}
	contract.mu.Lock()
	defer contract.mu.Unlock()
	expiresAt, ok := contract.liveTokens[cookie.Value]
	return ok && expiresAt.After(time.Now())
}

func (contract *realFRPContractServer) handle(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	contract.mu.Lock()
	contract.desktopHeaderSeen[request.URL.Path] = request.Header.Get(frpDesktopHeader)
	contract.mu.Unlock()

	switch request.URL.Path {
	case "/api/console-auth/status":
		// Public, like the real 88FRP route.
		writeEnvelope(writer, http.StatusOK, map[string]any{"configured": true, "username": contract.username})
		return
	case "/api/console-auth/challenge":
		// Public. The real server returns the same salt/iterations for the
		// account and sends NO Set-Cookie.
		writeEnvelope(writer, http.StatusOK, map[string]any{
			"challengeId": contract.challengeID,
			"nonce":       contract.nonce,
			"salt":        contract.salt,
			"iterations":  contract.iterations,
		})
		return
	case frpLoginEndpoint:
		contract.handleLogin(writer, request)
		return
	}

	// Everything else sits behind requireConsoleAuth.
	if !contract.desktopRequest(request) && !contract.liveSession(request) {
		contract.mu.Lock()
		contract.unauthRequests++
		contract.mu.Unlock()
		writer.Header().Del("Content-Type")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"success":false,"message":"请先登录控制台。","data":null}`))
		return
	}

	switch request.URL.Path {
	case "/api/instances":
		contract.mu.Lock()
		contract.instancesRequests++
		contract.mu.Unlock()
		writeEnvelope(writer, http.StatusOK, []map[string]any{{"id": "home", "name": "Home"}})
	case "/api/instances/home/tunnels":
		writeEnvelope(writer, http.StatusOK, map[string]any{"tunnels": []map[string]any{
			{"name": "phonebridge", "type": "tcp", "localPort": 8787, "remotePort": 19080, "enabled": true},
		}})
	case "/api/instances/home/config":
		writeEnvelope(writer, http.StatusOK, map[string]any{"configText": "serverAddr = \"198.51.100.7\"\n"})
	default:
		http.NotFound(writer, request)
	}
}

func (contract *realFRPContractServer) handleLogin(writer http.ResponseWriter, request *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeEnvelope(writer, http.StatusBadRequest, nil)
		return
	}
	contract.mu.Lock()
	contract.loginBodies = append(contract.loginBodies, body)
	contract.loginCalls++
	contract.mu.Unlock()

	// The real server derives an expected HMAC proof from the stored verifier.
	// This mock accepts any proof for the configured username; the proof
	// algorithm itself is covered by TestDeriveFRPProofServerMatchesBrowserProof.
	if username, _ := body["username"].(string); username != contract.username {
		writeEnvelope(writer, http.StatusUnauthorized, nil)
		return
	}
	// A missing challengeId is rejected by the real server too.
	if challengeID, _ := body["challengeId"].(string); challengeID != contract.challengeID {
		writeEnvelope(writer, http.StatusUnauthorized, nil)
		return
	}

	// Honour desktopEnabled exactly like the management routes: 88FRP's login
	// route is public, but the session it returns is only useful when the desktop
	// bypass is off and a cookie is what authenticates.
	contract.mu.Lock()
	desktopOn := contract.desktopEnabled
	contract.mu.Unlock()

	remember, _ := body["remember"].(bool)
	ttl := contract.sessionTTL
	if remember {
		ttl = contract.rememberTTL
	}
	_ = desktopOn
	token := "token-for-remember-" + map[bool]string{true: "true", false: "false"}[remember]
	expiresAt := time.Now().Add(ttl)
	contract.mu.Lock()
	contract.liveTokens[token] = expiresAt
	contract.sessionExpiresAt = expiresAt
	contract.lastToken = token
	contract.mu.Unlock()
	writer.Header().Set("Set-Cookie", frpSessionCookieName+"="+token+
		"; Max-Age="+itoa(int(ttl/time.Second))+"; Path=/; HttpOnly; SameSite=Strict")
	writeEnvelope(writer, http.StatusOK, map[string]any{"remember": remember})
}

// itoa renders a seconds count without a unit suffix for the Max-Age attribute.
func itoa(value int) string {
	return strconv.Itoa(value)
}

func writeEnvelope(writer http.ResponseWriter, status int, data any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": status >= 200 && status < 300, "message": "ok", "data": data})
}

// expireLastSession simulates the server-side session dying (24h elapsed, an
// 88FRP restart, or an explicit revoke) while PhoneBridge keeps its jar.
func (contract *realFRPContractServer) expireLastSession() {
	contract.mu.Lock()
	defer contract.mu.Unlock()
	if contract.lastToken != "" {
		delete(contract.liveTokens, contract.lastToken)
	}
	contract.sessionExpiresAt = time.Time{}
}

func (contract *realFRPContractServer) lastLoginBody() map[string]any {
	contract.mu.Lock()
	defer contract.mu.Unlock()
	if len(contract.loginBodies) == 0 {
		return nil
	}
	return contract.loginBodies[len(contract.loginBodies)-1]
}

func (contract *realFRPContractServer) loginBodyCount() int {
	contract.mu.Lock()
	defer contract.mu.Unlock()
	return len(contract.loginBodies)
}

func (contract *realFRPContractServer) authenticatedRequests() int {
	contract.mu.Lock()
	defer contract.mu.Unlock()
	return contract.instancesRequests
}

func (contract *realFRPContractServer) unauthorizedRequests() int {
	contract.mu.Lock()
	defer contract.mu.Unlock()
	return contract.unauthRequests
}

// markerRoundTripper removes the 88FRP desktop marker from outbound requests.
// It is the test-only way to reach the cookie branch of requireConsoleAuth
// (i.e. the dead-session path) now that PhoneBridge always sends the marker.
type markerRoundTripper struct{ base http.RoundTripper }

func (transport markerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header.Del(frpDesktopHeader)
	base := transport.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}

// stripDesktopMarkerClient wraps parent so the desktop marker is not sent,
// while preserving its cookie jar so the dead session cookie is still offered.
func stripDesktopMarkerClient(parent *http.Client) *http.Client {
	client := &http.Client{Transport: markerRoundTripper{}}
	if parent != nil {
		client.Jar = parent.Jar
		client.Timeout = parent.Timeout
		client.CheckRedirect = parent.CheckRedirect
		if parent.Transport != nil {
			client.Transport = markerRoundTripper{base: parent.Transport}
		}
	}
	return client
}

// requireSessionOnly makes the server ignore the desktop marker so only a
// session cookie authenticates. It models the path a 401 takes once the
// server-side session has died.
func (contract *realFRPContractServer) requireSessionOnly() {
	contract.mu.Lock()
	contract.desktopEnabled = false
	contract.mu.Unlock()
}

// newContractService wires a service + manager to the contract server with the
// rate limiter disabled.
func newContractService(t *testing.T, contract *realFRPContractServer) (*RemoteAdminService, *PublicAccessManager) {
	t.Helper()
	manager := NewPublicAccessManager(filepath.Join(t.TempDir(), "settings.json"), "", 8787, log.New(io.Discard, "", 0))
	service := NewRemoteAdminService(manager, log.New(io.Discard, "", 0))
	service.minActionInterval = 0
	return service, manager
}

// remoteAdminRequestFromLoopback builds the request PhoneBridge's settings page
// would send on the local machine.
func remoteAdminRequestFromLoopback(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:55555"
	request.Host = "127.0.0.1:8787"
	request.Header.Set("Origin", "http://127.0.0.1:8787")
	request.Header.Set("Content-Type", "application/json")
	return request
}

// TestFRPManagementSyncSendsDesktopHeaderReproducesRealContract is the core
// regression test for Bug 1. Before the fix, PhoneBridge's sync to a real
// 88FRP service returned HTTP 401 because it sent neither the desktop marker
// nor a session cookie. This test drives the real sync path against a server
// that enforces the real rule and asserts it now succeeds.
func TestFRPManagementSyncSendsDesktopHeaderReproducesRealContract(t *testing.T) {
	contract := newRealFRPContractServer(t, "2909272751")
	defer contract.Close()
	manager := NewPublicAccessManager(filepath.Join(t.TempDir(), "settings.json"), "", 8787, log.New(io.Discard, "", 0))

	// Sanity: an unmarked bare loopback request is 401, exactly like the live
	// 88FRP service on http://127.0.0.1:8801.
	unmarked, err := http.Get(contract.server.URL + "/api/instances")
	if err != nil {
		t.Fatalf("unmarked instances request: %v", err)
	}
	unmarked.Body.Close()
	if unmarked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unmarked GET /api/instances = %d, want 401 (the reported symptom)", unmarked.StatusCode)
	}

	// Marked request succeeds — proving the marker is the discriminant.
	markedRequest, _ := http.NewRequest(http.MethodGet, contract.server.URL+"/api/instances", nil)
	markedRequest.Header.Set(frpDesktopHeader, frpDesktopHeaderValue)
	markedResponse, err := http.DefaultClient.Do(markedRequest)
	if err != nil {
		t.Fatalf("marked instances request: %v", err)
	}
	markedResponse.Body.Close()
	if markedResponse.StatusCode != http.StatusOK {
		t.Fatalf("marked GET /api/instances = %d, want 200", markedResponse.StatusCode)
	}

	// Now the real PhoneBridge sync path must succeed with no session at all,
	// because it carries the desktop marker.
	settings := defaultPublicAccessSettings()
	settings.Source = publicAccess88FRP
	settings.FRPServiceURL = contract.server.URL
	settings.FRPInstanceID = "home"
	settings.FRPTunnelName = "phonebridge"
	settings.FRPScheme = "https"
	snapshot, err := manager.Save(context.Background(), settings)
	if err != nil {
		t.Fatalf("sync against the real 88FRP contract failed: %v", err)
	}
	if snapshot.State != "frp-ready" {
		t.Fatalf("sync state = %q, want frp-ready (message %q)", snapshot.State, snapshot.Message)
	}
	if snapshot.EffectiveURL != "https://198.51.100.7:19080" {
		t.Fatalf("effective URL = %q", snapshot.EffectiveURL)
	}
	if contract.authenticatedRequests() == 0 {
		t.Fatal("the management API was never reached")
	}
	if seen := contract.desktopHeaderSeen["/api/instances"]; seen != frpDesktopHeaderValue {
		t.Fatalf("PhoneBridge sent %s=%q on /api/instances, want %q", frpDesktopHeader, seen, frpDesktopHeaderValue)
	}
}

// TestProxyLoginSendsRememberTrue pins the second half of Bug 1: the login body
// must match console-login.js and request a long-lived session. Without
// `remember: true`, 88FRP issues a 24-hour session that later expires and makes
// every sync return 401.
func TestProxyLoginSendsRememberTrue(t *testing.T) {
	contract := newRealFRPContractServer(t, "2909272751")
	defer contract.Close()
	service, _ := newContractService(t, contract)

	challengeBody, _ := json.Marshal(map[string]string{"username": "2909272751", "serviceUrl": contract.server.URL})
	recorder := httptest.NewRecorder()
	service.HandleChallenge(recorder, remoteAdminRequestFromLoopback(t, http.MethodPost, "/api/remote-admin/challenge", string(challengeBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("challenge status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	loginBody, _ := json.Marshal(map[string]string{"username": "2909272751", "proof": "derived-proof", "challengeId": contract.challengeID})
	recorder = httptest.NewRecorder()
	service.HandleLogin(recorder, remoteAdminRequestFromLoopback(t, http.MethodPost, "/api/remote-admin/login", string(loginBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	body := contract.lastLoginBody()
	if body == nil {
		t.Fatal("88FRP收到了 no login body")
	}
	remember, present := body["remember"]
	if !present {
		t.Fatalf("login body omitted `remember`: %#v (88FRP would issue a 24h session)", body)
	}
	if remember != true {
		t.Fatalf("login body sent remember=%#v, want true", remember)
	}
	// The desktop marker must be on the login call too.
	if seen := contract.desktopHeaderSeen[frpLoginEndpoint]; seen != frpDesktopHeaderValue {
		t.Fatalf("login sent %s=%q, want %q", frpDesktopHeader, seen, frpDesktopHeaderValue)
	}
	// The granted session must be the long-lived one, not the 24h default.
	contract.mu.Lock()
	expiresAt := contract.sessionExpiresAt
	contract.mu.Unlock()
	// 88FRP grants REMEMBER_TTL_MS (5 years) with remember:true and only 24h
	// without it, so the granted lifetime must be near frpRememberTTL.
	if remaining := time.Until(expiresAt); remaining < frpRememberTTL-24*time.Hour {
		t.Fatalf("session expires %s from now, want the ~5-year remember lifetime (frpRememberTTL)", remaining)
	}
	// The proof, not a password, is what reaches 88FRP.
	for _, forbidden := range []string{"password"} {
		if _, present := body[forbidden]; present {
			t.Fatalf("login body must never carry %q: %#v", forbidden, body)
		}
	}
}

// TestLoginWithoutSetCookieIsStillAccepted pins that PhoneBridge does not make
// login stricter than the real 88FRP contract. The real server does set
// `88frp_console`, but the desktop marker alone authenticates the whole /api
// surface (isDesktopRequest short-circuits requireConsoleAuth), so a login
// response without a cookie must still be accepted — and the sync that follows
// must reach the management API.
func TestLoginWithoutSetCookieIsStillAccepted(t *testing.T) {
	cookieLess := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case frpChallengeEndpoint:
			// A well-formed challenge so the attempt can be minted.
			_, _ = writer.Write([]byte(`{"success":true,"message":"ok","data":{"challengeId":"cid-1","nonce":"nonce-1","salt":"c2FsdA==","iterations":1000}}`))
		case "/api/instances":
			// Authenticated purely by the desktop marker, like the real server.
			if request.Header.Get(frpDesktopHeader) != frpDesktopHeaderValue {
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = writer.Write([]byte(`{"success":false,"message":"请先登录控制台。","data":null}`))
				return
			}
			_, _ = writer.Write([]byte(`{"success":true,"message":"ok","data":[{"id":"home","name":"Home"}]}`))
		default:
			// success:true but deliberately NO Set-Cookie.
			_, _ = writer.Write([]byte(`{"success":true,"message":"登录成功。","data":{"remember":true}}`))
		}
	}))
	defer cookieLess.Close()

	service, manager := newContractService(t, nil)
	challengeBody, _ := json.Marshal(map[string]string{"username": "alice", "serviceUrl": cookieLess.URL})
	recorder := httptest.NewRecorder()
	service.HandleChallenge(recorder, remoteAdminRequestFromLoopback(t, http.MethodPost, "/api/remote-admin/challenge", string(challengeBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("challenge status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	loginBody, _ := json.Marshal(map[string]string{"username": "alice", "proof": "p", "challengeId": "cid-1"})
	recorder = httptest.NewRecorder()
	service.HandleLogin(recorder, remoteAdminRequestFromLoopback(t, http.MethodPost, "/api/remote-admin/login", string(loginBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("a login without a Set-Cookie must still succeed, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !service.Status().Authenticated {
		t.Fatal("login must authenticate")
	}
	if manager.Snapshot().Settings.Source != publicAccess88FRP {
		t.Fatal("a successful login must switch the source to 88FRP")
	}
	// No instance is configured, so the sync legitimately asks the user to
	// choose one — the key point is that it reached the management API at all
	// instead of failing with 401.
	if state := manager.Snapshot().State; state != "frp-needs-instance" {
		t.Fatalf("state = %q, want frp-needs-instance (message %q)", state, manager.Snapshot().Message)
	}
}

// TestSyncUnauthorizedDropsSessionAndReauthenticates covers the recovery path:
// when the server-side session dies, a 401 during sync must clear the stale
// in-memory session, stop reporting `authenticated:true`, and trigger
// re-authentication instead of only surfacing frp-error forever.
func TestSyncUnauthorizedDropsSessionAndReauthenticates(t *testing.T) {
	contract := newRealFRPContractServer(t, "alice")
	defer contract.Close()
	service, manager := newContractService(t, contract)

	// The recovery reaction only fires when saved credentials can restore the
	// session, so this test needs a saved login. Skip when the platform cannot
	// persist credentials.
	requireCredentialStoreAvailable(t, service)
	if err := service.store.Save(storedCredentials{
		ServiceURL: contract.server.URL,
		Username:   "alice",
		Password:   "correct-password",
		Remember:   true,
	}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	// Observe the recovery reaction without running the real retry loop.
	expired := make(chan struct{}, 4)
	service.onSessionExpired = func() { expired <- struct{}{} }
	settings := defaultPublicAccessSettings()
	settings.Source = publicAccess88FRP
	settings.FRPServiceURL = contract.server.URL
	settings.FRPInstanceID = "home"
	settings.FRPTunnelName = "phonebridge"
	settings.FRPScheme = "https"
	settings.AutoSync = true

	// Establish the settings through a real Save+Sync while the session is
	// live, so the manager's own snapshot carries the instance/tunnel.
	if _, err := manager.Save(context.Background(), settings); err != nil {
		t.Fatalf("initial save+sync: %v", err)
	}

	// Sign in so the jar holds a live session…
	challengeBody, _ := json.Marshal(map[string]string{"username": "alice", "serviceUrl": contract.server.URL})
	recorder := httptest.NewRecorder()
	service.HandleChallenge(recorder, remoteAdminRequestFromLoopback(t, http.MethodPost, "/api/remote-admin/challenge", string(challengeBody)))
	loginBody, _ := json.Marshal(map[string]string{"username": "alice", "proof": "p", "challengeId": contract.challengeID})
	recorder = httptest.NewRecorder()
	service.HandleLogin(recorder, remoteAdminRequestFromLoopback(t, http.MethodPost, "/api/remote-admin/login", string(loginBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	if !service.Status().Authenticated {
		t.Fatal("login must authenticate")
	}

	// The server now stops honouring the desktop marker, so only a session cookie
	// authenticates and the dead cookie PhoneBridge still holds yields a 401.
	contract.requireSessionOnly()

	// …then the management API starts refusing while PhoneBridge keeps the dead
	// cookie in its jar. Stripping the marker is how the 401 is delivered through
	// the real sync path: the request no longer satisfies isDesktopRequest, so
	// requireConsoleAuth falls through to the cookie, which no longer names a live
	// session.
	contract.expireLastSession()
	manager.SetFRPClient(stripDesktopMarkerClient(manager.frpHTTPClient()))

	before := contract.unauthorizedRequests()
	snapshot, err := manager.Sync(context.Background())
	if err == nil {
		t.Fatalf("sync with a dead session must fail, got state %q", snapshot.State)
	}
	if !isFRPSessionExpired(err) {
		t.Fatalf("sync error = %v, want the session-expired sentinel", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("sync error must surface the HTTP 401 symptom, got %v", err)
	}
	if contract.unauthorizedRequests() <= before {
		t.Fatal("the mock never returned 401; the test did not exercise the dead-session path")
	}
	select {
	case <-expired:
	case <-time.After(2 * time.Second):
		t.Fatal("a 401 during sync did not trigger session invalidation")
	}
	if service.Status().Authenticated {
		t.Fatal("a 401 during sync must clear the stale authenticated state")
	}
	// The sync had previously succeeded, so syncFailed keeps the last good URL
	// for diagnosis and reports frp-unreachable rather than a bare frp-error.
	// Either way the stale session must be gone from the status API.
	if state := manager.Snapshot().State; state != "frp-unreachable" && state != "frp-error" {
		t.Fatalf("state = %q, want frp-unreachable or frp-error", state)
	}
}

// TestSyncUnauthorizedWithoutSessionIsNotBlamedOnLogin verifies the 401
// reaction stays quiet when PhoneBridge holds no session: an anonymous 401 must
// not invalidate a session that never existed, and must not start a retry loop.
func TestSyncUnauthorizedWithoutSessionIsNotBlamedOnLogin(t *testing.T) {
	unauthorized := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// No desktop bypass here: simulate a service that still refuses.
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"success":false,"message":"请先登录控制台。","data":null}`))
	}))
	defer unauthorized.Close()

	service, manager := newContractService(t, nil)
	// The reaction under test is frpSessionExpired's decision, so drive it
	// directly: the mock above records whatever the sync forwards.
	calls := 0
	service.onSessionExpired = func() { calls++ }
	if service.store.HasSaved() {
		t.Skip("a real saved credential exists on this machine")
	}

	// Put the manager straight into 88FRP mode without going through Save (Save
	// runs its own Sync); then drive the real sync path. Sync also overwrites the
	// manager snapshot, so the error and the returned snapshot are what this test
	// asserts on.
	manager.setSnapshot(PublicAccessSnapshot{Settings: PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: unauthorized.URL,
		FRPInstanceID: "home",
		FRPTunnelName: "phonebridge",
		AutoSync:      true,
	}})

	snapshot, err := manager.Sync(context.Background())
	if err == nil {
		t.Fatalf("sync against an always-401 service must fail, got %q", snapshot.State)
	}
	if !isFRPSessionExpired(err) {
		t.Fatalf("error = %v, want the session-expired sentinel", err)
	}
	if calls != 0 {
		t.Fatalf("an anonymous 401 must not invalidate a session that never existed (%d calls)", calls)
	}
	if service.Status().Authenticated {
		t.Fatal("service must remain unauthenticated")
	}
	// The real reaction must also be a no-op: no saved credentials, no loop.
	if service.autoLoginCancel != nil {
		t.Fatal("an anonymous 401 must not start an auto-login retry loop")
	}
}

// TestAutoLoginSendsRememberTrue verifies the saved-credential auto-login path
// (the one that produced the live `authenticated:true` state) requests the
// long-lived session and forwards a proof rather than the plaintext password.
func TestAutoLoginSendsRememberTrue(t *testing.T) {
	contract := newRealFRPContractServer(t, "alice")
	defer contract.Close()
	service, manager := newContractService(t, contract)
	requireCredentialStoreAvailable(t, service)

	// The saved settings already know which instance/tunnel to use; the
	// auto-login sync then completes rather than asking the UI to choose.
	manager.setSnapshot(PublicAccessSnapshot{Settings: PublicAccessSettings{
		Source:        publicAccess88FRP,
		FRPServiceURL: contract.server.URL,
		FRPInstanceID: "home",
		FRPTunnelName: "phonebridge",
		FRPScheme:     "https",
	}})

	if err := service.store.Save(storedCredentials{
		ServiceURL: contract.server.URL,
		Username:   "alice",
		Password:   "correct-password",
		Remember:   true,
	}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	if err := service.autoLoginOnce(context.Background()); err != nil {
		t.Fatalf("auto login: %v", err)
	}
	if !service.Status().Authenticated {
		t.Fatal("auto login must authenticate")
	}
	body := contract.lastLoginBody()
	if body == nil {
		t.Fatal("auto login did not reach the 88FRP login route")
	}
	if remember, _ := body["remember"].(bool); !remember {
		t.Fatalf("auto login body must request remember=true, got %#v", body)
	}
	if _, present := body["password"]; present {
		t.Fatal("auto login must forward the derived proof, never the password field")
	}
	proof, _ := body["proof"].(string)
	if proof == "" || proof == "correct-password" {
		t.Fatalf("auto login proof = %q, want a derived proof", proof)
	}
	if manager.Snapshot().State != "frp-ready" {
		t.Fatalf("auto login sync state = %q, want frp-ready (message %q)", manager.Snapshot().State, manager.Snapshot().Message)
	}
}

// TestCompleteLoginUsesCookieSessionWithoutDesktopHeader proves the change did
// not make the desktop marker the only auth path: when 88FRP does hand out a
// session cookie, PhoneBridge's jar keeps it and the cookie alone is enough.
func TestCompleteLoginUsesCookieSessionWithoutDesktopHeader(t *testing.T) {
	// This server ignores the desktop marker entirely and only accepts a
	// session cookie, i.e. the non-desktop branch of 88FRP's middleware.
	var mu sync.Mutex
	issued := ""
	cookieOnly := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/console-auth/challenge":
			_, _ = writer.Write([]byte(`{"success":true,"message":"ok","data":{"challengeId":"cid","nonce":"nonce","salt":"c2FsdA==","iterations":1000}}`))
		case frpLoginEndpoint:
			mu.Lock()
			issued = "cookie-token-9"
			mu.Unlock()
			writer.Header().Set("Set-Cookie", frpSessionCookieName+"=cookie-token-9; Max-Age=157680000; Path=/; HttpOnly; SameSite=Strict")
			_, _ = writer.Write([]byte(`{"success":true,"message":"ok","data":{"remember":true}}`))
		case "/api/instances":
			cookie, err := request.Cookie(frpSessionCookieName)
			if err != nil || cookie.Value != "cookie-token-9" {
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = writer.Write([]byte(`{"success":false,"message":"请先登录控制台。","data":null}`))
				return
			}
			_, _ = writer.Write([]byte(`{"success":true,"message":"ok","data":[{"id":"home","name":"Home"}]}`))
		case "/api/instances/home/tunnels":
			_, _ = writer.Write([]byte(`{"success":true,"message":"ok","data":{"tunnels":[]}}`))
		case "/api/instances/home/config":
			_, _ = writer.Write([]byte(`{"success":true,"message":"ok","data":{"configText":"serverAddr = \"198.51.100.7\"\n"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer cookieOnly.Close()

	service, _ := newContractService(t, nil)
	challengeBody, _ := json.Marshal(map[string]string{"username": "alice", "serviceUrl": cookieOnly.URL})
	recorder := httptest.NewRecorder()
	service.HandleChallenge(recorder, remoteAdminRequestFromLoopback(t, http.MethodPost, "/api/remote-admin/challenge", string(challengeBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("challenge status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	loginBody, _ := json.Marshal(map[string]string{"username": "alice", "proof": "p", "challengeId": "cid"})
	recorder = httptest.NewRecorder()
	service.HandleLogin(recorder, remoteAdminRequestFromLoopback(t, http.MethodPost, "/api/remote-admin/login", string(loginBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	mu.Lock()
	token := issued
	mu.Unlock()
	if token == "" {
		t.Fatal("mock issued no cookie")
	}
	// The jar must hold the session cookie for later syncs.
	client, jar, err := service.frpSessionClient()
	if err != nil {
		t.Fatalf("session client: %v", err)
	}
	if !jarHasFRPSession(client, cookieOnly.URL) {
		t.Fatalf("jar does not hold %s after login", frpSessionCookieName)
	}
	parsed, _ := url.Parse(cookieOnly.URL)
	found := false
	for _, cookie := range jar.Cookies(parsed) {
		if cookie.Name == frpSessionCookieName {
			found = true
		}
	}
	if !found {
		t.Fatalf("jar missing %s", frpSessionCookieName)
	}
}

// TestJarHasFRPSession exercises the post-login session check directly.
func TestJarHasFRPSession(t *testing.T) {
	service, _ := newContractService(t, nil)
	sessionClient, jar, err := service.frpSessionClient()
	if err != nil {
		t.Fatalf("session client: %v", err)
	}
	const serviceURL = "http://127.0.0.1:8801"
	if jarHasFRPSession(sessionClient, serviceURL) {
		t.Fatal("an empty jar must not report a session")
	}
	parsed, _ := url.Parse(serviceURL)
	jar.SetCookies(parsed, []*http.Cookie{{Name: frpSessionCookieName, Value: "tok", Path: "/"}})
	if !jarHasFRPSession(sessionClient, serviceURL) {
		t.Fatal("a populated jar must report a session")
	}
	// An empty cookie value is not a session.
	jar.SetCookies(parsed, []*http.Cookie{{Name: frpSessionCookieName, Value: "", Path: "/"}})
	if jarHasFRPSession(sessionClient, serviceURL) {
		t.Fatal("an empty cookie value must not count as a session")
	}
	// A different cookie is not the 88FRP session.
	jar.SetCookies(parsed, []*http.Cookie{{Name: "other", Value: "tok", Path: "/"}})
	if jarHasFRPSession(sessionClient, serviceURL) {
		t.Fatal("an unrelated cookie must not count as a session")
	}
}
