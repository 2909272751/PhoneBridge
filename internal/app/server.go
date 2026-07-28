package app

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
)

//go:embed web/*
var embeddedWeb embed.FS

const version = "1.0.0"

type Config struct {
	ListenAddress string
	DemoMode      bool
	ADBPath       string
	ScrcpyPath    string
	PublicBaseURL string
	SettingsPath  string
	ICEServers    []ICEServerConfig
	ICEPort       int
}

type ICEServerConfig struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type Server struct {
	config        Config
	logger        *log.Logger
	mu            sync.RWMutex
	adb           ADBSnapshot
	scrcpy        ScrcpySnapshot
	sessions      map[string]*Session
	sessionToken  map[string]string
	webrtcPeers   map[string]*webRTCPeer
	activeID      string
	publicAccess  *PublicAccessManager
	webRTCAPI     *pion.API
	iceConn       *net.UDPConn
	icePublicPort int
	controlMu     sync.Mutex
	controllers   map[string]*adbControlShell
}

func New(config Config, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	scrcpy := discoverScrcpy(config.ScrcpyPath)
	if config.ADBPath == "" {
		config.ADBPath = bundledADBPath(scrcpy.Path)
	}
	if config.ADBPath == "" {
		config.ADBPath = "adb"
	}
	if config.SettingsPath == "" {
		config.SettingsPath = defaultSettingsPath()
	}
	return &Server{
		config:       config,
		logger:       logger,
		scrcpy:       scrcpy,
		sessions:     make(map[string]*Session),
		sessionToken: make(map[string]string),
		webrtcPeers:  make(map[string]*webRTCPeer),
		controllers:  make(map[string]*adbControlShell),
		publicAccess: NewPublicAccessManager(config.SettingsPath, config.PublicBaseURL, listenPort(config.ListenAddress), logger),
	}
}

func (server *Server) Run() error {
	defer server.closeControlShells()
	server.refreshDevices(context.Background())
	runtimeContext, cancelRuntime := context.WithCancel(context.Background())
	defer cancelRuntime()
	server.publicAccess.Start(runtimeContext)
	server.startPublicICE(runtimeContext)
	defer func() {
		if server.iceConn != nil {
			_ = server.iceConn.Close()
		}
	}()
	mux, err := server.routes()
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              server.config.ListenAddress,
		Handler:           withSecurityHeaders(server.logger, mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       75 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	err = httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// startPublicICE binds WebRTC to one local UDP socket and advertises the
// corresponding 88FRP public UDP mapping. This preserves WebRTC's native
// RTP/UDP media path without sending screen frames over the HTTP tunnel.
func (server *Server) startPublicICE(ctx context.Context) {
	port := server.config.ICEPort
	if port == 0 {
		port = 3478
	}
	host, publicPort, err := server.publicAccess.DiscoverUDPForward(ctx, port)
	if err != nil {
		server.logger.Printf("WebRTC public UDP mapping unavailable: %v", err)
		return
	}
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		server.logger.Printf("WebRTC UDP socket unavailable on %d: %v", port, err)
		return
	}
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: connection})
	settingEngine := pion.SettingEngine{}
	settingEngine.SetICEUDPMux(mux)
	settingEngine.SetNAT1To1IPs([]string{host}, pion.ICECandidateTypeHost)
	server.webRTCAPI = pion.NewAPI(pion.WithSettingEngine(settingEngine))
	server.iceConn = connection
	server.icePublicPort = publicPort
	server.logger.Printf("WebRTC UDP mapped through 88FRP: %s:%d -> 127.0.0.1:%d", host, publicPort, port)
}

func (server *Server) routes() (http.Handler, error) {
	webRoot, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	fileServer := http.FileServer(http.FS(webRoot))

	mux.Handle("GET /assets/", http.StripPrefix("/assets/", fileServer))
	mux.HandleFunc("GET /healthz", server.handleHealth)
	mux.HandleFunc("GET /api/status", server.handleStatus)
	mux.HandleFunc("GET /api/public-access", server.handlePublicAccess)
	mux.HandleFunc("PUT /api/public-access", server.handleSavePublicAccess)
	mux.HandleFunc("POST /api/public-access/sync", server.handleSyncPublicAccess)
	mux.HandleFunc("POST /api/devices/refresh", server.handleRefreshDevices)
	mux.HandleFunc("POST /api/sessions", server.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/current", server.handleCurrentSession)
	mux.HandleFunc("POST /api/sessions/{id}/stop", server.handleStopSession)
	mux.HandleFunc("GET /api/public/sessions/{token}", server.handlePublicSession)
	mux.HandleFunc("POST /api/public/sessions/{token}/join", server.handleJoinSession)
	mux.HandleFunc("POST /api/public/sessions/{token}/events", server.handleControlEvent)
	mux.HandleFunc("POST /api/public/sessions/{token}/stream-profile", server.handleStreamProfile)
	mux.HandleFunc("GET /api/public/sessions/{token}/frame", server.handleFrame)
	mux.HandleFunc("GET /api/public/sessions/{token}/webrtc/config", server.handleWebRTCConfig)
	mux.HandleFunc("POST /api/public/sessions/{token}/webrtc/offer", server.handleWebRTCOffer)
	mux.HandleFunc("GET /s/{token}", func(writer http.ResponseWriter, request *http.Request) {
		server.servePage(writer, request, webRoot, "controller.html")
	})
	mux.HandleFunc("GET /", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(writer, request)
			return
		}
		server.servePage(writer, request, webRoot, "index.html")
	})
	return mux, nil
}

func (server *Server) servePage(writer http.ResponseWriter, request *http.Request, webRoot fs.FS, name string) {
	content, err := fs.ReadFile(webRoot, name)
	if err != nil {
		http.Error(writer, "页面资源缺失", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	_, _ = writer.Write(content)
}

func (server *Server) refreshDevices(ctx context.Context) ADBSnapshot {
	snapshot := discoverADB(ctx, server.config.ADBPath, server.config.DemoMode)
	server.mu.Lock()
	server.adb = snapshot
	server.mu.Unlock()
	return snapshot
}

func (server *Server) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": version,
		"time":    time.Now(),
	})
}

func (server *Server) handleStatus(writer http.ResponseWriter, request *http.Request) {
	server.mu.Lock()
	server.refreshSessionsLocked(time.Now())
	var current *Session
	var webrtc *webRTCPeerStats
	if server.activeID != "" {
		if session := server.sessions[server.activeID]; session != nil {
			value := publicOwnerSession(*session)
			current = &value
		}
		if peer := server.webrtcPeers[server.activeID]; peer != nil {
			value := peer.snapshotStats()
			webrtc = &value
		}
	}
	snapshot := server.adb
	server.mu.Unlock()
	publicAccess := server.publicAccess.Snapshot()
	cloudState := "local-ready"
	cloudMessage := "尚未设置公网地址；分享链接仅能在本机访问。"
	if publicAccess.State == "manual-ready" || publicAccess.State == "frp-ready" {
		cloudState = "public-ready"
		cloudMessage = publicAccess.Message
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"appName":       "PhoneBridge",
		"version":       version,
		"demoMode":      server.config.DemoMode,
		"agentState":    "running",
		"cloudState":    cloudState,
		"cloudMessage":  cloudMessage,
		"adb":           snapshot,
		"scrcpy":        server.scrcpy,
		"activeSession": current,
		"webrtc":        webrtc,
		"origin":        requestOrigin(request),
		"publicAccess":  publicAccess,
	})
}

func (server *Server) handlePublicAccess(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, server.publicAccess.Snapshot())
}

func (server *Server) handleSavePublicAccess(writer http.ResponseWriter, request *http.Request) {
	var input PublicAccessSettings
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "公网分享设置无效")
		return
	}
	value, err := server.publicAccess.Save(request.Context(), input)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, value)
}

func (server *Server) handleSyncPublicAccess(writer http.ResponseWriter, request *http.Request) {
	value, err := server.publicAccess.Sync(request.Context())
	if err != nil {
		writeError(writer, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, value)
}

func (server *Server) handleRefreshDevices(writer http.ResponseWriter, request *http.Request) {
	snapshot := server.refreshDevices(request.Context())
	writeJSON(writer, http.StatusOK, snapshot)
}

func (server *Server) handleCreateSession(writer http.ResponseWriter, request *http.Request) {
	var input CreateSessionRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "请求内容无效")
		return
	}
	if input.DurationMinute < 0 || input.DurationMinute > 24*60 {
		writeError(writer, http.StatusBadRequest, "分享时长无效")
		return
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	server.refreshSessionsLocked(time.Now())
	if server.activeID != "" {
		active := server.sessions[server.activeID]
		if active != nil && active.State != "stopped" && active.State != "expired" {
			if active.DeviceID == input.DeviceID {
				// PhoneBridge is intentionally a single-phone, single-controller
				// tool. Returning the existing link keeps it stable when the owner
				// reopens the share dialog instead of creating dead, competing URLs.
				writeJSON(writer, http.StatusOK, publicOwnerSession(*active))
				return
			}
			writeError(writer, http.StatusConflict, "已有其他手机分享正在进行，请先停止当前分享")
			return
		}
	}

	var device *Device
	for index := range server.adb.Devices {
		if server.adb.Devices[index].ID == input.DeviceID {
			value := server.adb.Devices[index]
			device = &value
			break
		}
	}
	if device == nil {
		writeError(writer, http.StatusNotFound, "未找到所选手机，请刷新设备列表")
		return
	}
	if device.State != "device" {
		writeError(writer, http.StatusConflict, deviceStateMessage(device.State))
		return
	}

	session, err := newSession(input, *device, server.shareOrigin(request), time.Now())
	if err != nil {
		server.logger.Printf("创建随机会话失败：%v", err)
		writeError(writer, http.StatusInternalServerError, "无法安全创建分享会话")
		return
	}
	server.sessions[session.ID] = &session
	server.sessionToken[session.Token] = session.ID
	server.activeID = session.ID
	writeJSON(writer, http.StatusCreated, publicOwnerSession(session))
}

func (server *Server) handleCurrentSession(writer http.ResponseWriter, _ *http.Request) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.refreshSessionsLocked(time.Now())
	if server.activeID == "" {
		writeJSON(writer, http.StatusOK, map[string]any{"activeSession": nil})
		return
	}
	session := server.sessions[server.activeID]
	if session == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"activeSession": nil})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"activeSession": publicOwnerSession(*session)})
}

func (server *Server) handleStopSession(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	server.mu.Lock()
	session := server.sessions[id]
	if session == nil {
		server.mu.Unlock()
		writeError(writer, http.StatusNotFound, "分享会话不存在")
		return
	}
	now := time.Now()
	session.State = "stopped"
	session.ViewerState = "disconnected"
	session.ConnectionMode = "not-connected"
	session.StoppedAt = &now
	if server.activeID == id {
		server.activeID = ""
	}
	peer := server.webrtcPeers[id]
	delete(server.webrtcPeers, id)
	response := publicOwnerSession(*session)
	server.mu.Unlock()
	if peer != nil {
		peer.close()
	}
	writeJSON(writer, http.StatusOK, response)
}

func (server *Server) handlePublicSession(writer http.ResponseWriter, request *http.Request) {
	session, ok := server.sessionByToken(request.PathValue("token"))
	if !ok {
		writeError(writer, http.StatusNotFound, "分享链接无效或已经结束")
		return
	}
	writeJSON(writer, http.StatusOK, publicGuestSession(session, false))
}

func (server *Server) handleJoinSession(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "访问码格式无效")
		return
	}

	token := request.PathValue("token")
	server.mu.Lock()
	defer server.mu.Unlock()
	session, ok := server.sessionByTokenLocked(token, time.Now())
	if !ok {
		writeError(writer, http.StatusGone, "分享链接已失效")
		return
	}
	if session.RequireCode && strings.TrimSpace(input.Code) != session.AccessCode {
		writeError(writer, http.StatusUnauthorized, "访问码不正确")
		return
	}
	// A browser refresh, mobile background resume, or switching networks must
	// not consume the single-controller slot forever. The latest holder of the
	// secret link replaces the old WebRTC peer when it sends a new offer.
	if session.ViewerState == "connected" {
		writeJSON(writer, http.StatusOK, publicGuestSession(*session, true))
		return
	}
	session.ViewerState = "connected"
	session.State = "connected"
	if session.IsDemo {
		session.ConnectionMode = "demo"
	} else {
		session.ConnectionMode = "fallback-screen"
		// The first fallback frame is a direct Android screenshot. Wake the
		// USB-connected phone so that a sleeping display is not sent as a
		// perfectly valid, but completely black, PNG to the remote viewer.
		deviceID := session.DeviceID
		go func() {
			_ = wakeDevice(context.Background(), server.config.ADBPath, deviceID)
		}()
	}
	writeJSON(writer, http.StatusOK, publicGuestSession(*session, true))
}

func (server *Server) handleControlEvent(writer http.ResponseWriter, request *http.Request) {
	var event ControlEvent
	if err := decodeJSON(request, &event); err != nil {
		writeError(writer, http.StatusBadRequest, "控制事件无效")
		return
	}
	if event.Type == "" {
		writeError(writer, http.StatusBadRequest, "缺少控制事件类型")
		return
	}

	server.mu.Lock()
	session, ok := server.sessionByTokenLocked(request.PathValue("token"), time.Now())
	if !ok {
		server.mu.Unlock()
		writeError(writer, http.StatusGone, "分享链接已失效")
		return
	}
	if session.ViewerState != "connected" || session.Mode != "control" {
		server.mu.Unlock()
		writeError(writer, http.StatusForbidden, "当前会话没有控制权限")
		return
	}
	now := time.Now()
	session.LastEventAt = &now
	var start *PointerPoint
	if event.Type == "pointer-down" {
		session.pointerStart = &PointerPoint{X: event.X, Y: event.Y}
	}
	if event.Type == "pointer-up" {
		start = session.pointerStart
		session.pointerStart = nil
	}
	deviceID, demo := session.DeviceID, session.IsDemo
	peer := server.webrtcPeers[session.ID]
	server.mu.Unlock()
	if !demo {
		if peer != nil && peer.sendNativeControl(event) == nil {
			writeJSON(writer, http.StatusAccepted, map[string]any{"accepted": true, "demo": false})
			return
		}
		if err := server.dispatchControl(deviceID, event, start); err != nil {
			writeError(writer, http.StatusBadGateway, err.Error())
			return
		}
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{
		"accepted": true,
		"demo":     demo,
	})
}

func (server *Server) handleFrame(writer http.ResponseWriter, request *http.Request) {
	server.mu.Lock()
	session, ok := server.sessionByTokenLocked(request.PathValue("token"), time.Now())
	if !ok {
		server.mu.Unlock()
		writeError(writer, http.StatusGone, "分享链接已失效")
		return
	}
	if session.ViewerState != "connected" {
		server.mu.Unlock()
		writeError(writer, http.StatusForbidden, "当前浏览器尚未加入会话")
		return
	}
	deviceID, demo := session.DeviceID, session.IsDemo
	server.mu.Unlock()
	if demo {
		writeError(writer, http.StatusNotFound, "演示会话没有真实手机画面")
		return
	}
	// The HTTP compatibility path is deliberately compressed. A full PNG from a
	// 1080x2340 phone is often several MiB, which makes a public share look like
	// a frozen image even though capture keeps succeeding.
	frame, contentType, err := devicePreview(request.Context(), server.config.ADBPath, deviceID)
	if err != nil {
		writeError(writer, http.StatusBadGateway, err.Error())
		return
	}
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("Cache-Control", "no-store, max-age=0")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = writer.Write(frame)
}

func (server *Server) sessionByToken(token string) (Session, bool) {
	server.mu.Lock()
	defer server.mu.Unlock()
	session, ok := server.sessionByTokenLocked(token, time.Now())
	if !ok {
		return Session{}, false
	}
	return *session, true
}

func (server *Server) sessionByTokenLocked(token string, now time.Time) (*Session, bool) {
	id := server.sessionToken[token]
	if id == "" {
		return nil, false
	}
	session := server.sessions[id]
	if session == nil {
		return nil, false
	}
	session.refresh(now)
	if session.State == "stopped" || session.State == "expired" {
		return nil, false
	}
	return session, true
}

func (server *Server) refreshSessionsLocked(now time.Time) {
	for _, session := range server.sessions {
		session.refresh(now)
	}
	if server.activeID != "" {
		active := server.sessions[server.activeID]
		if active == nil || active.State == "stopped" || active.State == "expired" {
			server.activeID = ""
		}
	}
}

func publicOwnerSession(session Session) Session {
	return session
}

func publicGuestSession(session Session, joined bool) map[string]any {
	response := map[string]any{
		"id":             session.ID,
		"deviceName":     session.DeviceName,
		"state":          session.State,
		"mode":           session.Mode,
		"requireCode":    session.RequireCode,
		"allowClipboard": session.AllowClipboard,
		"allowAudio":     session.AllowAudio,
		"isDemo":         session.IsDemo,
		"viewerState":    session.ViewerState,
		"connectionMode": session.ConnectionMode,
		"createdAt":      session.CreatedAt,
		"expiresAt":      session.ExpiresAt,
		"joined":         joined,
	}
	return response
}

func requestOrigin(request *http.Request) string {
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	if forwarded := request.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	host := request.Host
	if forwarded := request.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

func (server *Server) shareOrigin(request *http.Request) string {
	if value := strings.TrimSpace(server.publicAccess.EffectiveURL()); value != "" {
		return strings.TrimRight(value, "/")
	}
	return requestOrigin(request)
}

func decodeJSON(request *http.Request, target any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(nil, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{"error": message})
}

func deviceStateMessage(state string) string {
	switch state {
	case "unauthorized":
		return "手机尚未授权，请在手机上确认 USB 调试授权"
	case "offline":
		return "手机当前离线，请重新连接 USB 或重启 ADB"
	default:
		return "手机当前不可用：" + state
	}
}

func withSecurityHeaders(logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		// Session state changes during a live share. Browsers are allowed to
		// cache GET responses unless told otherwise, which could leave the owner
		// page showing an already-stopped session (or hide a just-created one).
		if strings.HasPrefix(request.URL.Path, "/api/") {
			writer.Header().Set("Cache-Control", "no-store, max-age=0")
		}
		writer.Header().Set("Content-Security-Policy", strings.Join([]string{
			"default-src 'self'",
			"script-src 'self'",
			"style-src 'self'",
			"img-src 'self' data:",
			"connect-src 'self'",
			"media-src 'self' blob:",
			"object-src 'none'",
			"base-uri 'none'",
			"frame-ancestors 'none'",
		}, "; "))
		next.ServeHTTP(writer, request)
		if !strings.HasPrefix(request.URL.Path, "/assets/") {
			logger.Printf("%s %s (%s)", request.Method, path.Clean(request.URL.Path), time.Since(started).Round(time.Millisecond))
		}
	})
}
