package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// challengeTTL bounds how long a proxied 88FRP challenge stays usable for
	// its login attempt (SPEC: challenge 与当前 attempt 绑定且短时失效).
	challengeTTL = 60 * time.Second
	// remoteAdminBodyLimit caps management API request bodies.
	remoteAdminBodyLimit = 64 << 10
	// remoteAdminMinInterval rate-limits management actions (challenge,
	// login, logout) so a remote peer cannot hammer them.
	remoteAdminMinInterval = 3 * time.Second
	// frpPBKDF2Iterations is the PBKDF2 work factor when the 88FRP challenge
	// response does not specify one. The browser derives the key with the
	// challenge-provided value; this default only guards a malformed response.
	frpPBKDF2Iterations = 10000
	// frpPBKDF2KeyLength is the derived key length (SHA-256 output size).
	frpPBKDF2KeyLength = 32
)

// LocalAdminStatus is the non-secret information the main /api/status exposes
// about the 88FRP sign-in state. It deliberately contains no username,
// password, proof, cookie, file path or ciphertext: only booleans and the
// auto-login state enum (idle/running/done/failed).
type LocalAdminStatus struct {
	Authenticated bool   `json:"authenticated"`
	Saved         bool   `json:"saved"`
	AutoLoginState string `json:"autoLoginState,omitempty"`
}

// frpChallengeAttempt is the server-side memory of one 88FRP challenge. It is
// bound to the current login attempt: it carries the username and service URL
// it was issued for, expires quickly, and is consumed by the login call so a
// proof cannot be replayed.
type frpChallengeAttempt struct {
	challengeID string
	username    string
	challenge   string
	iterations  int
	nonce       string
	serviceURL  string
	expiresAt   time.Time
}

// RemoteAdminService hosts the 88FRP sign-in state for PhoneBridge. It runs
// on the main service mux (same origin as the settings page, no separate
// loopback listener): the browser signs in to 88FRP with a derived proof and
// the public settings API is usable from the local page and the forwarded
// public address alike — there is no admin code and no bearer authorization.
//
// The 88FRP session cookie jar and challenge state are in-memory only.
// Nothing is persisted, logged or echoed back; the plaintext 88FRP password
// never reaches this process through the browser path (the browser derives
// the proof with WebCrypto and the login API has no password field). The only
// exception is the optional local credential store, which persists the login
// encrypted (Windows DPAPI) for automatic sign-in after a restart.
type RemoteAdminService struct {
	logger       *log.Logger
	publicAccess *PublicAccessManager
	store        *credentialStore

	mu            sync.Mutex
	cookieJar     http.CookieJar
	frpClient     *http.Client
	serviceURL    string
	authenticated bool
	lastActionAt  time.Time
	// minActionInterval is the rate limit between management actions. It is
	// derived from remoteAdminMinInterval; tests may lower it.
	minActionInterval time.Duration

	attempt *frpChallengeAttempt

	// autoLoginState is the startup auto-login state: idle (no saved
	// credentials / not supported), running, done, or failed. autoLoginErr
	// carries only a log-safe summary and is never exposed over the API.
	autoLoginState string
	autoLoginErr   string
}

// NewRemoteAdminService creates the 88FRP sign-in service. The 88FRP session
// state lives only in memory and is served on the main mux; the optional
// credential store is derived from the public-access settings path.
func NewRemoteAdminService(publicAccess *PublicAccessManager, logger *log.Logger) *RemoteAdminService {
	if logger == nil {
		logger = log.Default()
	}
	service := &RemoteAdminService{
		logger:            logger,
		publicAccess:      publicAccess,
		minActionInterval: remoteAdminMinInterval,
		autoLoginState:    "idle",
	}
	if publicAccess != nil {
		service.store = newCredentialStore(publicAccess.SettingsPath())
	}
	return service
}

// Status returns the public, secret-free sign-in state for /api/status.
// It only reports booleans and the auto-login state enum — never the saved
// username, password, file path or ciphertext.
func (service *RemoteAdminService) Status() LocalAdminStatus {
	service.mu.Lock()
	authenticated := service.authenticated
	autoLoginState := service.autoLoginState
	service.mu.Unlock()
	saved := false
	if service.store != nil {
		saved = service.store.HasSaved()
	}
	return LocalAdminStatus{
		Authenticated:  authenticated,
		Saved:          saved,
		AutoLoginState: autoLoginState,
	}
}

// sameOrigin is the CSRF guard for the public management API: a browser
// request must come from the same origin as the Host header, so a third-party
// page cannot drive the API through a victim browser.
func (service *RemoteAdminService) sameOrigin(request *http.Request) bool {
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	return parsed.Host == request.Host
}

// requireRemoteJSON enforces content type and same-origin for the public
// management API before any handler reads the body.
func (service *RemoteAdminService) requireRemoteJSON(writer http.ResponseWriter, request *http.Request) bool {
	if !strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/json") {
		writeJSON(writer, http.StatusUnsupportedMediaType, map[string]any{"error": "仅接受 JSON 请求"})
		return false
	}
	if !service.sameOrigin(request) {
		writeJSON(writer, http.StatusForbidden, map[string]any{"error": "拒绝来自跨站页面的管理请求"})
		return false
	}
	return true
}

// acquireActionSlot enforces the management-action rate limit and records the
// attempt time. Every action — successful or not — refreshes the slot so a
// remote peer cannot hammer the endpoints.
func (service *RemoteAdminService) acquireActionSlot() bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	now := time.Now()
	if !service.lastActionAt.IsZero() && now.Sub(service.lastActionAt) < service.minActionInterval {
		return false
	}
	service.lastActionAt = now
	return true
}

// decodeRemoteJSON decodes a management request body with a hard size limit
// and strict unknown-field rejection: the 88FRP login API has no password
// field by construction, and a client that sends one is rejected outright.
func decodeRemoteJSON(request *http.Request, target any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(nil, request.Body, remoteAdminBodyLimit))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// HandleChallenge proxies the 88FRP v3 console-auth challenge and stores the
// attempt server-side so the login call is bound to it. The browser receives
// only what it needs to derive the proof with WebCrypto.
func (service *RemoteAdminService) HandleChallenge(writer http.ResponseWriter, request *http.Request) {
	if !service.requireRemoteJSON(writer, request) {
		return
	}
	if !service.acquireActionSlot() {
		writeJSON(writer, http.StatusTooManyRequests, map[string]any{"error": "操作过于频繁，请稍后再试"})
		return
	}
	var input struct {
		Username   string `json:"username"`
		ServiceURL string `json:"serviceUrl"`
	}
	if err := decodeRemoteJSON(request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "登录请求无效"})
		return
	}
	username := strings.TrimSpace(input.Username)
	if username == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "请输入 88FRP 用户名"})
		return
	}
	serviceURL := strings.TrimSpace(input.ServiceURL)
	if serviceURL == "" {
		serviceURL = defaultPublicAccessSettings().FRPServiceURL
	}
	serviceURL, err := validateLocalFRPService(serviceURL)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	defer cancel()
	client, _, err := service.frpSessionClient()
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": "无法初始化登录会话"})
		return
	}
	attempt, err := service.proxyChallenge(ctx, client, serviceURL, username)
	if err != nil {
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": safeFRPLoginError("88FRP 登录失败", err)})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"challengeId": attempt.challengeID,
		"challenge":   attempt.challenge,
		"iterations":  attempt.iterations,
		"nonce":       attempt.nonce,
	})
}

// HandleLogin completes the 88FRP console-auth flow with the browser-derived
// proof. The request body carries username, proof and challengeId only — there
// is no password field, and an unknown field (including "password") is
// rejected by decodeRemoteJSON. The server's Set-Cookie is captured by the
// in-memory jar, then the public access source is switched to 88FRP and synced
// (instances, TCP 8787 tunnel, UDP 3478 discovery, persisted mapping, ICE
// refresh). The challenge attempt is consumed regardless of the outcome.
//
// Login deliberately bypasses the shared 3-second action slot: a challenge and
// the login that immediately follows it must not be blocked by the same
// interval (SPEC: 正常配对 login 不受 challenge 间隔阻断). Abuse is still
// bounded because login only runs with a matching, unexpired, one-shot attempt,
// which can only be minted through the rate-limited challenge endpoint; a login
// with no attempt / a stale or mismatched challenge is rejected quickly and
// never leaves an authenticated session.
func (service *RemoteAdminService) HandleLogin(writer http.ResponseWriter, request *http.Request) {
	if !service.requireRemoteJSON(writer, request) {
		return
	}
	var input struct {
		Username    string `json:"username"`
		Proof       string `json:"proof"`
		ChallengeID string `json:"challengeId"`
	}
	if err := decodeRemoteJSON(request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "登录请求无效"})
		return
	}
	username := strings.TrimSpace(input.Username)
	proof := strings.TrimSpace(input.Proof)
	challengeID := strings.TrimSpace(input.ChallengeID)
	if username == "" || proof == "" || challengeID == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "登录请求缺少用户名、证明或 challenge"})
		return
	}
	// Consume the challenge attempt: it is bound to this login and cannot be
	// replayed.
	service.mu.Lock()
	attempt := service.attempt
	service.attempt = nil
	service.mu.Unlock()
	if attempt == nil || attempt.challengeID != challengeID {
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": "登录 challenge 已失效，请重新开始登录"})
		return
	}
	if time.Now().After(attempt.expiresAt) {
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": "登录 challenge 已过期，请重新开始登录"})
		return
	}
	if attempt.username != username {
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": "登录 challenge 与用户名不匹配"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	defer cancel()
	client, _, err := service.frpSessionClient()
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": "无法初始化登录会话"})
		return
	}
	if err := service.proxyLogin(ctx, client, attempt.serviceURL, username, proof, challengeID); err != nil {
		// A failed login must not leave a half-authenticated cookie jar.
		service.clearFRPSession()
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": safeFRPLoginError("88FRP 登录失败", err)})
		return
	}
	service.mu.Lock()
	service.authenticated = true
	service.serviceURL = attempt.serviceURL
	service.mu.Unlock()
	snapshot, syncErr := service.completeFRPLogin(request.Context(), client, attempt.serviceURL)
	message := "已登录 88FRP 并同步"
	if syncErr != nil {
		message = "已登录 88FRP，但同步未完成：" + syncErr.Error()
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"success":       true,
		"authenticated": true,
		"message":       message,
		"publicAccess":  snapshot,
	})
}

// HandleLogout clears the in-memory 88FRP session. On the local machine it
// also deletes the saved credentials so a later restart does not silently
// sign back in (SPEC point 6). The public page can only end the in-memory
// session.
func (service *RemoteAdminService) HandleLogout(writer http.ResponseWriter, request *http.Request) {
	if !service.requireRemoteJSON(writer, request) {
		return
	}
	service.clearFRPSession()
	if service.localOriginOnly(request) && service.store != nil {
		if err := service.store.Delete(); err != nil && !errors.Is(err, ErrCredentialStoreUnsupported) {
			service.logger.Printf("退出登录时清除保存的 88FRP 凭据失败：%v", err)
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"success": true})
}

// HandleStatus reports the secret-free sign-in state (88FRP session,
// credential-save flag, auto-login state and the current public-access
// snapshot) for the settings page. It never returns username, password, file
// path or ciphertext (SPEC point 7).
func (service *RemoteAdminService) HandleStatus(writer http.ResponseWriter, request *http.Request) {
	status := service.Status()
	writeJSON(writer, http.StatusOK, map[string]any{
		"authenticated":  status.Authenticated,
		"saved":          status.Saved,
		"autoLoginState": status.AutoLoginState,
		"publicAccess":   service.publicAccess.Snapshot(),
	})
}

// HandleSaveCredentials is the loopback-only endpoint that persists the 88FRP
// login for automatic sign-in. It accepts serviceUrl/username/password/
// remember and requires a request from the local machine: a loopback peer
// with no forwarded headers and a loopback Host matching a loopback Origin.
// Public-tunnel requests and forged forwarded headers are rejected outright
// (SPEC point 2). The response never echoes any credential material.
func (service *RemoteAdminService) HandleSaveCredentials(writer http.ResponseWriter, request *http.Request) {
	if !service.localOriginOnly(request) {
		writeJSON(writer, http.StatusForbidden, map[string]any{"error": "仅允许在连接电脑的本机页面保存 88FRP 登录凭据"})
		return
	}
	if !strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/json") {
		writeJSON(writer, http.StatusUnsupportedMediaType, map[string]any{"error": "仅接受 JSON 请求"})
		return
	}
	var input struct {
		ServiceURL string `json:"serviceUrl"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		Remember   bool   `json:"remember"`
	}
	if err := decodeRemoteJSON(request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "保存凭据请求无效"})
		return
	}
	if service.store == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "当前平台不支持保存 88FRP 登录凭据"})
		return
	}
	if !input.Remember {
		// Unchecking "remember login" clears any previously saved credentials.
		if err := service.store.Delete(); err != nil && !errors.Is(err, ErrCredentialStoreUnsupported) {
			service.logger.Printf("清除保存的 88FRP 凭据失败：%v", err)
		}
		writeJSON(writer, http.StatusOK, map[string]any{"success": true, "saved": false})
		return
	}
	username := strings.TrimSpace(input.Username)
	password := input.Password
	if username == "" || password == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "请输入 88FRP 用户名和密码"})
		return
	}
	serviceURL := strings.TrimSpace(input.ServiceURL)
	if serviceURL == "" {
		serviceURL = defaultPublicAccessSettings().FRPServiceURL
	}
	serviceURL, err := validateLocalFRPService(serviceURL)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := service.store.Save(storedCredentials{ServiceURL: serviceURL, Username: username, Password: password, Remember: true}); err != nil {
		if errors.Is(err, ErrCredentialStoreUnsupported) {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "当前平台不支持保存 88FRP 登录凭据"})
			return
		}
		service.logger.Printf("保存 88FRP 凭据失败（不记录凭据内容）：%v", err)
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": "无法保存 88FRP 登录凭据"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"success": true, "saved": true})
}

// localOriginOnly enforces the local-machine origin guard used by the
// credential endpoints: the request must arrive from a loopback peer, carry
// no forwarded headers (a local browser never sets them), and present a
// loopback Host with a matching loopback Origin. Requests arriving through
// the 88FRP public tunnel are rejected even when they land on 127.0.0.1,
// because the tunnel forwards the public Host and Origin.
func (service *RemoteAdminService) localOriginOnly(request *http.Request) bool {
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP", "X-Real-Host"} {
		if strings.TrimSpace(request.Header.Get(header)) != "" {
			return false
		}
	}
	remoteHost, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		remoteHost = request.RemoteAddr
	}
	if ip := net.ParseIP(remoteHost); ip == nil || !ip.IsLoopback() {
		return false
	}
	host := strings.TrimSpace(request.Host)
	if host == "" {
		return false
	}
	hostname := host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		hostname = parsedHost
	}
	hostIP := net.ParseIP(hostname)
	hostIsLoopback := strings.EqualFold(hostname, "localhost") || (hostIP != nil && hostIP.IsLoopback())
	if !hostIsLoopback {
		return false
	}
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return parsed.Host == host
}

// --- 88FRP session and proxying ---

// frpSessionClient returns the in-memory cookie-jar HTTP client shared by the
// challenge and login calls, creating it on first use. The jar keeps the 88FRP
// session alive only for the lifetime of the PhoneBridge process.
func (service *RemoteAdminService) frpSessionClient() (*http.Client, http.CookieJar, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.cookieJar == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, nil, err
		}
		service.cookieJar = jar
	}
	if service.frpClient == nil {
		service.frpClient = &http.Client{Timeout: 6 * time.Second, Jar: service.cookieJar}
	}
	return service.frpClient, service.cookieJar, nil
}

// proxyChallenge performs the first step of the 88FRP v3.0 console-auth flow
// and stores the attempt server-side, bound to this username and service URL.
func (service *RemoteAdminService) proxyChallenge(ctx context.Context, client *http.Client, serviceURL, username string) (*frpChallengeAttempt, error) {
	// 88FRP v3 issues the challenge as a bare GET with no body, no
	// Content-Type and no username: the server returns the same challenge for
	// any account, and username is bound to the attempt here in PhoneBridge.
	// Sending a JSON POST body makes 88FRP return HTTP 401.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, serviceURL+"/api/console-auth/challenge", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("88FRP 本机服务返回 HTTP %d", response.StatusCode)
	}
	var envelope frpEnvelope
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("无法读取 88FRP challenge：%w", err)
	}
	if !envelope.Success {
		if envelope.Message == "" {
			envelope.Message = "用户名或密码不正确"
		}
		return nil, errors.New(envelope.Message)
	}
	var challengeData struct {
		ChallengeID string `json:"challengeId"`
		Challenge   string `json:"challenge"`
		Salt        string `json:"salt"`
		Iterations  int    `json:"iterations"`
		Nonce       string `json:"nonce"`
	}
	if err := json.Unmarshal(envelope.Data, &challengeData); err != nil {
		return nil, fmt.Errorf("88FRP challenge 数据格式无效：%w", err)
	}
	challenge := firstNonEmpty(challengeData.Salt, challengeData.Challenge)
	challengeID := strings.TrimSpace(challengeData.ChallengeID)
	nonce := strings.TrimSpace(challengeData.Nonce)
	if challenge == "" || challengeID == "" || nonce == "" {
		return nil, errors.New("88FRP 未返回完整的登录 challenge")
	}
	iterations := challengeData.Iterations
	if iterations <= 0 {
		iterations = frpPBKDF2Iterations
	}
	attempt := &frpChallengeAttempt{
		challengeID: challengeID,
		username:    username,
		challenge:   challenge,
		iterations:  iterations,
		nonce:       nonce,
		serviceURL:  serviceURL,
		expiresAt:   time.Now().Add(challengeTTL),
	}
	service.mu.Lock()
	service.attempt = attempt
	service.mu.Unlock()
	return attempt, nil
}

// proxyLogin completes the 88FRP console-auth flow with username, proof and
// challengeId. The plaintext password never leaves the browser; this process
// only forwards the derived proof. The server's Set-Cookie is captured by the
// in-memory jar for subsequent API calls.
func (service *RemoteAdminService) proxyLogin(ctx context.Context, client *http.Client, serviceURL, username, proof, challengeID string) error {
	body, err := json.Marshal(map[string]string{
		"username":    username,
		"proof":       proof,
		"challengeId": challengeID,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, serviceURL+"/api/console-auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("88FRP 本机服务返回 HTTP %d", response.StatusCode)
	}
	var envelope frpEnvelope
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("无法读取 88FRP 登录结果：%w", err)
	}
	if !envelope.Success {
		if envelope.Message == "" {
			envelope.Message = "用户名或密码不正确"
		}
		return errors.New(envelope.Message)
	}
	return nil
}

// clearFRPSession drops the in-memory 88FRP session and detaches it from the
// public-access manager so later syncs fall back to the anonymous client.
func (service *RemoteAdminService) clearFRPSession() {
	service.mu.Lock()
	service.cookieJar = nil
	service.frpClient = nil
	service.serviceURL = ""
	service.authenticated = false
	service.mu.Unlock()
	service.publicAccess.SetFRPClient(nil)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// safeFRPLoginError prefixes a login failure with the 88FRP-provided message
// (server-generated; never echoes the password, proof or cookies).
func safeFRPLoginError(prefix string, err error) string {
	if err == nil {
		return prefix
	}
	return prefix + "：" + err.Error()
}

// --- automatic sign-in from the saved credentials ---

// completeFRPLogin switches the public-access source to 88FRP, attaches the
// authenticated client and runs the automatic Save+Sync (instances, TCP
// tunnel, UDP forward mapping, ICE refresh). It is shared by the browser
// login and the background auto-login.
func (service *RemoteAdminService) completeFRPLogin(ctx context.Context, client *http.Client, serviceURL string) (PublicAccessSnapshot, error) {
	service.publicAccess.SetFRPClient(client)
	settings := service.publicAccess.Snapshot().Settings
	settings.Source = publicAccess88FRP
	settings.FRPServiceURL = serviceURL
	if settings.FRPScheme == "" {
		settings.FRPScheme = "http"
	}
	return service.publicAccess.Save(ctx, settings)
}

// decodeBase64Salt decodes an 88FRP v3 challenge salt that the server sends
// as base64 or base64url (with or without padding) into the raw bytes PBKDF2
// consumes. The real 88FRP console-login.js calls bytes(challenge.salt), i.e.
// it base64/base64url-decodes the salt before deriving the key. It never
// falls back to UTF-8: an undecodable salt returns an error so a protocol
// mismatch is surfaced instead of silently producing a wrong proof (SPEC
// point 4).
func decodeBase64Salt(salt string) ([]byte, error) {
	trimmed := strings.TrimSpace(salt)
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if raw, err := encoding.DecodeString(trimmed); err == nil {
			return raw, nil
		}
	}
	return nil, errors.New("88FRP salt 不是有效的 base64/base64url")
}

// deriveFRPProofServer derives the 88FRP v3 proof exactly like the settings
// page: PBKDF2-SHA256 derives a 32-byte key from the password and the
// base64/base64url-decoded challenge salt, HMAC-SHA256 signs
// `${nonce}\n${username.trim()}`, and the result is base64url-encoded (SPEC
// point 5). It is used only by the background auto-login, where the saved
// password is available.
func deriveFRPProofServer(password, username, challenge string, iterations int, nonce string) (string, error) {
	if iterations <= 0 {
		iterations = frpPBKDF2Iterations
	}
	salt, err := decodeBase64Salt(challenge)
	if err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, frpPBKDF2KeyLength)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(nonce + "\n" + strings.TrimSpace(username)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// startAutoLogin asynchronously restores the saved 88FRP login on startup
// without blocking the main service. On failure it retries with exponential
// backoff until the context ends. Only a log-safe summary of the failure is
// kept; the status API never sees it (SPEC point 4).
func (service *RemoteAdminService) startAutoLogin(ctx context.Context) {
	go func() {
		delay := 5 * time.Second
		for {
			err := service.autoLoginOnce(ctx)
			if err == nil {
				return
			}
			if ctx.Err() != nil {
				return
			}
			service.logger.Printf("88FRP 自动登录失败，将在 %s 后重试（仅记录摘要）：%v", delay, safeAutoLoginSummary(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			if delay < 5*time.Minute {
				delay *= 2
			}
		}
	}()
}

// autoLoginOnce performs one real 88FRP challenge/proof/login/sync using the
// saved credentials and reports the outcome through autoLoginState. It never
// blocks the main service and never exposes credential material.
func (service *RemoteAdminService) autoLoginOnce(ctx context.Context) error {
	service.mu.Lock()
	service.autoLoginState = "running"
	service.autoLoginErr = ""
	service.mu.Unlock()
	if service.store == nil {
		service.mu.Lock()
		service.autoLoginState = "idle"
		service.mu.Unlock()
		return nil
	}
	credentials, err := service.store.Load()
	if err != nil {
		service.mu.Lock()
		service.autoLoginState = "idle"
		service.mu.Unlock()
		if !errors.Is(err, errNoSavedCredentials) && !errors.Is(err, ErrCredentialStoreUnsupported) {
			service.logger.Printf("读取保存的 88FRP 凭据失败（仅记录摘要）：%v", err)
		}
		return nil
	}
	service.mu.Lock()
	alreadyAuthenticated := service.authenticated
	service.mu.Unlock()
	if alreadyAuthenticated {
		service.mu.Lock()
		service.autoLoginState = "done"
		service.mu.Unlock()
		return nil
	}
	serviceURL, err := service.runAutoLogin(ctx, credentials)
	if err != nil {
		service.mu.Lock()
		service.autoLoginState = "failed"
		service.autoLoginErr = safeAutoLoginSummary(err)
		service.mu.Unlock()
		return err
	}
	service.mu.Lock()
	service.authenticated = true
	service.serviceURL = serviceURL
	service.autoLoginState = "done"
	service.autoLoginErr = ""
	service.mu.Unlock()
	return nil
}

// runAutoLogin drives the real 88FRP console-auth flow with the saved
// credentials: challenge, server-side proof derivation, login, and the
// automatic Save+Sync. The plaintext password exists only in this call.
func (service *RemoteAdminService) runAutoLogin(ctx context.Context, credentials storedCredentials) (string, error) {
	client, _, err := service.frpSessionClient()
	if err != nil {
		return "", err
	}
	attempt, err := service.proxyChallenge(ctx, client, credentials.ServiceURL, credentials.Username)
	if err != nil {
		service.clearFRPSession()
		return "", err
	}
	proof, err := deriveFRPProofServer(credentials.Password, credentials.Username, attempt.challenge, attempt.iterations, attempt.nonce)
	if err != nil {
		service.clearFRPSession()
		return "", err
	}
	if err := service.proxyLogin(ctx, client, attempt.serviceURL, attempt.username, proof, attempt.challengeID); err != nil {
		service.clearFRPSession()
		return "", err
	}
	// The session is authenticated; a sync failure is not fatal for
	// auto-login because the periodic public-access sync retries it.
	if _, err := service.completeFRPLogin(ctx, client, attempt.serviceURL); err != nil {
		service.logger.Printf("88FRP 自动登录后同步未完成（仅记录摘要）：%v", safeAutoLoginSummary(err))
	}
	return attempt.serviceURL, nil
}

// safeAutoLoginSummary returns a log-safe summary of an auto-login failure.
// The error messages in this path are server-generated (88FRP messages or
// HTTP statuses) and never carry credential material; the summary is still
// length-capped so a pathological response cannot flood the log.
func safeAutoLoginSummary(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > 200 {
		message = message[:200]
	}
	return message
}
