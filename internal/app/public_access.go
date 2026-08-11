package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	publicAccessManual = "manual"
	publicAccess88FRP  = "88frp"
)

// PublicAccessSettings contains only local sharing settings. 88FRP credentials
// deliberately remain owned by the already signed-in 88FRP desktop client.
type PublicAccessSettings struct {
	Source            string `json:"source"`
	ManualURL         string `json:"manualUrl"`
	FRPServiceURL     string `json:"frpServiceUrl"`
	FRPInstanceID     string `json:"frpInstanceId"`
	FRPTunnelName     string `json:"frpTunnelName"`
	FRPScheme         string `json:"frpScheme"`
	AutoSync          bool   `json:"autoSync"`
	FRPLastSuccessURL string `json:"frpLastSuccessUrl,omitempty"`
	// FRPUDPForward is the last successfully discovered 88FRP UDP mapping for
	// the local WebRTC ICE port. It stores only public endpoint metadata;
	// 88FRP credentials and cookies are never persisted here.
	FRPUDPForward *UDPForwardMapping `json:"frpUdpForward,omitempty"`
}

type PublicAccessSnapshot struct {
	Settings     PublicAccessSettings `json:"settings"`
	EffectiveURL string               `json:"effectiveUrl"`
	State        string               `json:"state"`
	Message      string               `json:"message"`
	LastSyncedAt *time.Time           `json:"lastSyncedAt,omitempty"`
	Instances    []FRPInstance        `json:"instances,omitempty"`
	Tunnels      []FRPTunnel          `json:"tunnels,omitempty"`
}

type FRPInstance struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type FRPTunnel struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Type        string `json:"type"`
	LocalPort   any    `json:"localPort"`
	RemotePort  any    `json:"remotePort"`
	Enabled     bool   `json:"enabled"`
}

// UDPForwardMapping is the last successfully discovered 88FRP UDP proxy
// mapping for the local WebRTC ICE port.
type UDPForwardMapping struct {
	PublicHost   string    `json:"publicHost"`
	PublicPort   int       `json:"publicPort"`
	LocalICEPort int       `json:"localIcePort"`
	TunnelName   string    `json:"tunnelName,omitempty"`
	DiscoveredAt time.Time `json:"discoveredAt"`
}

// validFor reports whether the mapping is structurally valid and targets the
// given local ICE port. A mapping is never invented: it is only returned when
// it was previously discovered and persisted by a successful 88FRP sync.
func (mapping *UDPForwardMapping) validFor(localPort int) bool {
	return mapping != nil &&
		strings.TrimSpace(mapping.PublicHost) != "" &&
		mapping.PublicPort >= 1 && mapping.PublicPort <= 65535 &&
		mapping.LocalICEPort == localPort
}

type publicAccessFile struct {
	Settings PublicAccessSettings `json:"publicAccess"`
	ProbeID  string               `json:"probeId,omitempty"`
}

type PublicAccessManager struct {
	mu        sync.RWMutex
	path      string
	localPort int
	logger    *log.Logger
	client    *http.Client
	snapshot  PublicAccessSnapshot
	probeID   string
	// udpLocalPort is the local WebRTC ICE port that the 88FRP UDP proxy
	// targets (default 3478). It is set by the server from config.ICEPort.
	udpLocalPort int
	// udpForwardListener is invoked after a fresh 88FRP UDP mapping is
	// persisted, so the server can advertise it without a restart.
	udpForwardListener func()
	// frpClient is the authenticated 88FRP HTTP client (in-memory cookie jar)
	// injected after a successful local login. When nil, the default anonymous
	// client is used, which matches the "already signed-in 88FRP desktop
	// client" behavior. It is never persisted.
	frpClientMu sync.RWMutex
	frpClient   *http.Client
}

func NewPublicAccessManager(settingsPath, initialURL string, localPort int, logger *log.Logger) *PublicAccessManager {
	if logger == nil {
		logger = log.Default()
	}
	manager := &PublicAccessManager{
		path:         settingsPath,
		localPort:    localPort,
		logger:       logger,
		client:       &http.Client{Timeout: 6 * time.Second},
		udpLocalPort: 3478,
		snapshot:     PublicAccessSnapshot{Settings: defaultPublicAccessSettings(), State: "local", Message: "使用本机地址；配置公网地址后新分享链接会自动更新。"},
	}
	if err := manager.load(initialURL); err != nil {
		logger.Printf("读取公网分享设置失败：%v", err)
	}
	return manager
}

func defaultPublicAccessSettings() PublicAccessSettings {
	return PublicAccessSettings{
		Source:        publicAccessManual,
		FRPServiceURL: "http://127.0.0.1:8801",
		FRPScheme:     "http",
	}
}

func defaultSettingsPath() string {
	directory, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(directory, "PhoneBridge", "settings.json")
}

func listenPort(address string) int {
	_, value, err := net.SplitHostPort(address)
	if err != nil {
		return 8787
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 8787
	}
	return port
}

func (manager *PublicAccessManager) load(initialURL string) error {
	settings := defaultPublicAccessSettings()
	if manager.path != "" {
		data, err := os.ReadFile(manager.path)
		if err == nil {
			var saved publicAccessFile
			if err := json.Unmarshal(data, &saved); err != nil {
				return fmt.Errorf("设置文件格式无效：%w", err)
			}
			settings = normalizePublicAccessSettings(saved.Settings)
			manager.probeID = strings.TrimSpace(saved.ProbeID)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if manager.probeID == "" {
		manager.probeID = generateProbeID()
	}
	if strings.TrimSpace(initialURL) != "" {
		settings.Source = publicAccessManual
		settings.ManualURL = strings.TrimSpace(initialURL)
	}
	manager.snapshot.Settings = settings
	if settings.Source == publicAccessManual && settings.ManualURL != "" {
		if value, err := normalizePublicURL(settings.ManualURL); err == nil {
			manager.snapshot.EffectiveURL = value
			manager.snapshot.State = "manual-ready"
			manager.snapshot.Message = "已使用手动公网地址；新创建的分享链接会使用该地址。"
		}
	} else if settings.Source == publicAccess88FRP && settings.FRPLastSuccessURL != "" {
		if value, err := normalizePublicURL(settings.FRPLastSuccessURL); err == nil {
			manager.snapshot.EffectiveURL = value
		}
	}
	// Persist once so the local probe ID (and any restored last-success URL)
	// is durable even before the user saves anything.
	return manager.persist(settings)
}

func normalizePublicAccessSettings(input PublicAccessSettings) PublicAccessSettings {
	defaults := defaultPublicAccessSettings()
	input.Source = strings.ToLower(strings.TrimSpace(input.Source))
	if input.Source != publicAccess88FRP {
		input.Source = publicAccessManual
	}
	input.ManualURL = strings.TrimSpace(input.ManualURL)
	input.FRPServiceURL = strings.TrimSpace(input.FRPServiceURL)
	if input.FRPServiceURL == "" {
		input.FRPServiceURL = defaults.FRPServiceURL
	}
	input.FRPInstanceID = strings.TrimSpace(input.FRPInstanceID)
	input.FRPTunnelName = strings.TrimSpace(input.FRPTunnelName)
	input.FRPScheme = strings.ToLower(strings.TrimSpace(input.FRPScheme))
	if input.FRPScheme != "https" {
		input.FRPScheme = "http"
	}
	input.FRPLastSuccessURL = strings.TrimSpace(input.FRPLastSuccessURL)
	return input
}

func normalizePublicURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("请输入完整的 http:// 或 https:// 公网地址")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("公网地址不能包含账号、查询参数或锚点")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return strings.TrimRight(parsed.String(), "/"), nil
}

func validateLocalFRPService(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("88FRP 本机服务地址无效")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("88FRP 本机服务地址只能包含协议、主机和端口")
	}
	host := parsed.Hostname()
	if !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback() {
		return "", fmt.Errorf("为保护本机凭据，只能连接本机的 88FRP 服务")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (manager *PublicAccessManager) Snapshot() PublicAccessSnapshot {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return clonePublicAccessSnapshot(manager.snapshot)
}

func clonePublicAccessSnapshot(value PublicAccessSnapshot) PublicAccessSnapshot {
	value.Instances = append([]FRPInstance(nil), value.Instances...)
	value.Tunnels = append([]FRPTunnel(nil), value.Tunnels...)
	return value
}

func (manager *PublicAccessManager) EffectiveURL() string {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.snapshot.EffectiveURL
}

// SetUDPForwardListener registers a callback invoked when a fresh 88FRP UDP
// mapping is discovered and persisted, so WebRTC can be advertised without
// restarting PhoneBridge.
func (manager *PublicAccessManager) SetUDPForwardListener(listener func()) {
	manager.mu.Lock()
	manager.udpForwardListener = listener
	manager.mu.Unlock()
}

// SettingsPath returns the settings file path backing this manager. It is
// used to derive the location of the dedicated encrypted credentials file.
func (manager *PublicAccessManager) SettingsPath() string {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.path
}

// SetFRPClient replaces the HTTP client used for authenticated 88FRP API
// calls. The client carries an in-memory cookie jar and is never persisted.
// A nil value restores the default anonymous client.
func (manager *PublicAccessManager) SetFRPClient(client *http.Client) {
	manager.frpClientMu.Lock()
	manager.frpClient = client
	manager.frpClientMu.Unlock()
}

func (manager *PublicAccessManager) frpHTTPClient() *http.Client {
	manager.frpClientMu.RLock()
	client := manager.frpClient
	manager.frpClientMu.RUnlock()
	if client == nil {
		return manager.client
	}
	return client
}

// SetUDPForwardLocalPort records the local WebRTC ICE port that 88FRP UDP
// proxies target. A zero value keeps the default 3478.
func (manager *PublicAccessManager) SetUDPForwardLocalPort(port int) {
	if port == 0 {
		port = 3478
	}
	manager.mu.Lock()
	manager.udpLocalPort = port
	manager.mu.Unlock()
}

func (manager *PublicAccessManager) udpLocalPortValue() int {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.udpLocalPort == 0 {
		return 3478
	}
	return manager.udpLocalPort
}

// DiscoverUDPForward returns the public endpoint of the enabled 88FRP UDP
// proxy targeting the local ICE port. It prefers a fresh result from the
// already authenticated local 88FRP companion. When the management API is
// unavailable (HTTP 401, timeout, malformed response) it may return the
// structurally valid persisted mapping marked stale (fresh=false), so WebRTC
// keeps working through recovery after a restart. It never invents a mapping.
func (manager *PublicAccessManager) DiscoverUDPForward(ctx context.Context, localPort int) (string, int, bool, error) {
	manager.mu.RLock()
	settings := manager.snapshot.Settings
	manager.mu.RUnlock()
	if settings.Source != publicAccess88FRP {
		return "", 0, false, errors.New("88FRP public sharing is not enabled")
	}
	serviceURL, err := validateLocalFRPService(settings.FRPServiceURL)
	if err != nil {
		return manager.restoreUDPForward(localPort, err)
	}
	if settings.FRPInstanceID == "" {
		return manager.restoreUDPForward(localPort, errors.New("88FRP instance is not selected"))
	}
	tunnels, configText, err := manager.fetchInstance(ctx, serviceURL, settings.FRPInstanceID)
	if err != nil {
		return manager.restoreUDPForward(localPort, err)
	}
	for _, tunnel := range tunnels {
		if tunnel.Enabled && strings.EqualFold(strings.TrimSpace(tunnel.Type), "udp") && portFromValue(tunnel.LocalPort) == localPort {
			host, hostErr := frpServerHost(configText)
			if hostErr != nil {
				return manager.restoreUDPForward(localPort, hostErr)
			}
			port := portFromValue(tunnel.RemotePort)
			if port < 1 || port > 65535 {
				return manager.restoreUDPForward(localPort, errors.New("88FRP UDP proxy has no valid public port"))
			}
			mapping := &UDPForwardMapping{
				PublicHost:   host,
				PublicPort:   port,
				LocalICEPort: localPort,
				TunnelName:   strings.TrimSpace(tunnel.Name),
				DiscoveredAt: time.Now(),
			}
			manager.saveUDPForward(mapping)
			return host, port, true, nil
		}
	}
	return manager.restoreUDPForward(localPort, fmt.Errorf("no enabled 88FRP UDP proxy targets local port %d", localPort))
}

// refreshUDPForward best-effort re-runs UDP discovery after an authenticated
// 88FRP sync so a fresh mapping is persisted and the server is notified.
func (manager *PublicAccessManager) refreshUDPForward(ctx context.Context) error {
	_, _, _, err := manager.DiscoverUDPForward(ctx, manager.udpLocalPortValue())
	return err
}

// restoreUDPForward falls back to the persisted UDP mapping when a fresh
// discovery failed. The mapping is returned marked stale; it is only used when
// structurally valid for the requested local ICE port.
func (manager *PublicAccessManager) restoreUDPForward(localPort int, discoveryErr error) (string, int, bool, error) {
	mapping := manager.udpForwardMapping()
	if !mapping.validFor(localPort) {
		return "", 0, false, discoveryErr
	}
	manager.logger.Printf("88FRP UDP 发现失败（%v），使用已持久化的恢复映射 %s:%d", discoveryErr, mapping.PublicHost, mapping.PublicPort)
	return mapping.PublicHost, mapping.PublicPort, false, nil
}

func (manager *PublicAccessManager) udpForwardMapping() *UDPForwardMapping {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.snapshot.Settings.FRPUDPForward
}

// saveUDPForward persists a freshly discovered mapping and notifies the server
// listener when the mapping actually changed.
func (manager *PublicAccessManager) saveUDPForward(mapping *UDPForwardMapping) {
	manager.mu.Lock()
	settings := manager.snapshot.Settings
	changed := !sameUDPForward(settings.FRPUDPForward, mapping)
	if changed {
		settings.FRPUDPForward = mapping
		manager.snapshot.Settings = settings
	}
	listener := manager.udpForwardListener
	manager.mu.Unlock()
	if !changed {
		return
	}
	if err := manager.persist(settings); err != nil {
		manager.logger.Printf("持久化 88FRP UDP 映射失败：%v", err)
	}
	if listener != nil {
		// Discovery may run while Server.ensurePublicICE holds its ICE lock.
		// Notify asynchronously so persisting a fresh mapping cannot deadlock
		// by synchronously re-entering ensurePublicICE through the listener.
		go listener()
	}
}

func sameUDPForward(left, right *UDPForwardMapping) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.PublicHost == right.PublicHost &&
		left.PublicPort == right.PublicPort &&
		left.LocalICEPort == right.LocalICEPort &&
		left.TunnelName == right.TunnelName
}

func (manager *PublicAccessManager) Save(ctx context.Context, input PublicAccessSettings) (PublicAccessSnapshot, error) {
	settings := normalizePublicAccessSettings(input)
	// FRPLastSuccessURL is internal recovery state and is intentionally not
	// submitted by the web form. Preserve it while saving the user's visible
	// settings so a management-API failure can still verify the last tunnel.
	manager.mu.RLock()
	currentSettings := manager.snapshot.Settings
	manager.mu.RUnlock()
	if settings.Source == publicAccess88FRP && settings.FRPLastSuccessURL == "" {
		settings.FRPLastSuccessURL = currentSettings.FRPLastSuccessURL
	}
	// Like FRPLastSuccessURL, the UDP mapping is internal recovery state and is
	// not submitted by the settings form. Never erase it on an ordinary save.
	if settings.Source == publicAccess88FRP && settings.FRPUDPForward == nil {
		settings.FRPUDPForward = currentSettings.FRPUDPForward
	}
	if settings.Source == publicAccessManual {
		if settings.ManualURL == "" {
			manager.mu.Lock()
			manager.snapshot = PublicAccessSnapshot{Settings: settings, State: "local", Message: "尚未设置公网地址；新分享链接会使用当前本机地址。"}
			manager.mu.Unlock()
			if err := manager.persist(settings); err != nil {
				return manager.Snapshot(), err
			}
			return manager.Snapshot(), nil
		}
		value, err := normalizePublicURL(settings.ManualURL)
		if err != nil {
			return manager.Snapshot(), err
		}
		settings.ManualURL = value
		manager.mu.Lock()
		manager.snapshot = PublicAccessSnapshot{Settings: settings, EffectiveURL: value, State: "manual-ready", Message: "已保存公网地址；新创建的分享链接会使用该地址。"}
		manager.mu.Unlock()
		if err := manager.persist(settings); err != nil {
			return manager.Snapshot(), err
		}
		return manager.Snapshot(), nil
	}

	if _, err := validateLocalFRPService(settings.FRPServiceURL); err != nil {
		return manager.Snapshot(), err
	}
	manager.mu.Lock()
	manager.snapshot = PublicAccessSnapshot{Settings: settings, State: "frp-pending", Message: "正在从本机 88FRP 同步可用隧道。"}
	manager.mu.Unlock()
	if err := manager.persist(settings); err != nil {
		return manager.Snapshot(), err
	}
	return manager.Sync(ctx)
}

func (manager *PublicAccessManager) persist(settings PublicAccessSettings) error {
	if manager.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(publicAccessFile{Settings: settings, ProbeID: manager.probeID}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(manager.path), 0700); err != nil {
		return err
	}
	return os.WriteFile(manager.path, data, 0600)
}

func (manager *PublicAccessManager) Start(ctx context.Context) {
	go func() {
		// Do not leave a configured public share looking local-only until the
		// first 15-second periodic refresh after an application restart.
		startup := manager.Snapshot()
		if startup.Settings.Source == publicAccess88FRP && startup.Settings.AutoSync {
			if _, err := manager.Sync(ctx); err != nil {
				manager.logger.Printf("88FRP startup synchronization failed: %v", err)
			}
		}
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				snapshot := manager.Snapshot()
				if snapshot.Settings.Source == publicAccess88FRP && snapshot.Settings.AutoSync {
					if _, err := manager.Sync(ctx); err != nil {
						manager.logger.Printf("88FRP 地址同步失败：%v", err)
					}
				}
			}
		}
	}()
}

func (manager *PublicAccessManager) Sync(ctx context.Context) (PublicAccessSnapshot, error) {
	manager.mu.RLock()
	settings := manager.snapshot.Settings
	manager.mu.RUnlock()
	if settings.Source != publicAccess88FRP {
		return manager.Snapshot(), nil
	}
	serviceURL, err := validateLocalFRPService(settings.FRPServiceURL)
	if err != nil {
		return manager.Snapshot(), err
	}
	instances, err := manager.fetchInstances(ctx, serviceURL)
	if err != nil {
		manager.syncFailed(settings, nil, nil, err)
		return manager.Snapshot(), err
	}
	if settings.FRPInstanceID == "" {
		manager.setSnapshot(PublicAccessSnapshot{Settings: settings, State: "frp-needs-instance", Message: "请选择运行 PhoneBridge 的 88FRP 实例。", Instances: instances})
		return manager.Snapshot(), nil
	}
	tunnels, configText, err := manager.fetchInstance(ctx, serviceURL, settings.FRPInstanceID)
	if err != nil {
		manager.syncFailed(settings, instances, nil, err)
		return manager.Snapshot(), err
	}
	if settings.FRPTunnelName == "" {
		manager.setSnapshot(PublicAccessSnapshot{Settings: settings, State: "frp-needs-tunnel", Message: fmt.Sprintf("请选择本地端口为 %d 的已启用 TCP 隧道。", manager.localPort), Instances: instances, Tunnels: tunnels})
		return manager.Snapshot(), nil
	}
	var chosen *FRPTunnel
	for index := range tunnels {
		if tunnels[index].Name == settings.FRPTunnelName {
			value := tunnels[index]
			chosen = &value
			break
		}
	}
	if chosen == nil {
		err = fmt.Errorf("所选 88FRP 隧道已不存在")
		manager.setSyncError(settings, instances, tunnels, err)
		return manager.Snapshot(), err
	}
	if !chosen.Enabled {
		err = fmt.Errorf("所选 88FRP 隧道尚未启用")
		manager.setSyncError(settings, instances, tunnels, err)
		return manager.Snapshot(), err
	}
	if portFromValue(chosen.LocalPort) != manager.localPort {
		err = fmt.Errorf("所选隧道的本地端口不是 PhoneBridge 的 %d", manager.localPort)
		manager.setSyncError(settings, instances, tunnels, err)
		return manager.Snapshot(), err
	}
	if !strings.EqualFold(strings.TrimSpace(chosen.Type), "tcp") {
		err = fmt.Errorf("目前仅支持将 TCP 隧道作为公网访问地址")
		manager.setSyncError(settings, instances, tunnels, err)
		return manager.Snapshot(), err
	}
	host, err := frpServerHost(configText)
	if err != nil {
		manager.setSyncError(settings, instances, tunnels, err)
		return manager.Snapshot(), err
	}
	remotePort := portFromValue(chosen.RemotePort)
	if remotePort < 1 || remotePort > 65535 {
		err = fmt.Errorf("所选隧道缺少有效的公网端口")
		manager.setSyncError(settings, instances, tunnels, err)
		return manager.Snapshot(), err
	}
	publicURL := (&url.URL{Scheme: settings.FRPScheme, Host: net.JoinHostPort(host, strconv.Itoa(remotePort))}).String()
	settings.FRPLastSuccessURL = publicURL
	if err := manager.persist(settings); err != nil {
		manager.logger.Printf("持久化 88FRP 公网地址失败：%v", err)
	}
	// A successful authenticated sync also refreshes the UDP forward mapping so
	// WebRTC can be advertised without restarting PhoneBridge.
	if err := manager.refreshUDPForward(ctx); err != nil {
		manager.logger.Printf("刷新 88FRP UDP 映射失败：%v", err)
	}
	// The UDP refresh above may have persisted a new mapping; read the settings
	// back so the snapshot below includes it.
	manager.mu.RLock()
	settings = manager.snapshot.Settings
	manager.mu.RUnlock()
	now := time.Now()
	manager.setSnapshot(PublicAccessSnapshot{Settings: settings, EffectiveURL: publicURL, State: "frp-ready", Message: fmt.Sprintf("已从 88FRP 同步：%s", publicURL), LastSyncedAt: &now, Instances: instances, Tunnels: tunnels})
	return manager.Snapshot(), nil
}

func (manager *PublicAccessManager) setSnapshot(snapshot PublicAccessSnapshot) {
	manager.mu.Lock()
	manager.snapshot = snapshot
	manager.mu.Unlock()
}

func (manager *PublicAccessManager) setSyncError(settings PublicAccessSettings, instances []FRPInstance, tunnels []FRPTunnel, err error) {
	current := manager.Snapshot()
	manager.setSnapshot(PublicAccessSnapshot{Settings: settings, EffectiveURL: current.EffectiveURL, State: "frp-error", Message: "88FRP 同步失败：" + err.Error(), LastSyncedAt: current.LastSyncedAt, Instances: instances, Tunnels: tunnels})
}

func (manager *PublicAccessManager) ProbeID() string {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.probeID
}

// syncFailed handles a 88FRP management-API failure (HTTP 401, timeout, or a
// malformed response). When a last successful public URL exists, it probes that
// URL's dedicated health endpoint before deciding whether the tunnel is still
// reachable by the current PhoneBridge instance.
func (manager *PublicAccessManager) syncFailed(settings PublicAccessSettings, instances []FRPInstance, tunnels []FRPTunnel, err error) {
	current := manager.Snapshot()
	oldURL := strings.TrimSpace(settings.FRPLastSuccessURL)
	if oldURL == "" {
		oldURL = strings.TrimSpace(current.EffectiveURL)
	}
	if oldURL == "" {
		manager.setSnapshot(PublicAccessSnapshot{Settings: settings, State: "frp-error", Message: "88FRP 同步失败：" + err.Error(), LastSyncedAt: current.LastSyncedAt, Instances: instances, Tunnels: tunnels})
		return
	}
	if manager.verifyPublicHealth(context.Background(), oldURL) {
		manager.setSnapshot(PublicAccessSnapshot{Settings: settings, EffectiveURL: oldURL, State: "frp-stale-ready", Message: fmt.Sprintf("公网可用，但 88FRP 自动同步需重新登录/暂时失败：%v。仍继续使用 %s。", err, oldURL), LastSyncedAt: current.LastSyncedAt, Instances: instances, Tunnels: tunnels})
		return
	}
	manager.setSnapshot(PublicAccessSnapshot{Settings: settings, EffectiveURL: oldURL, State: "frp-unreachable", Message: fmt.Sprintf("无法确认公网地址 %s 仍指向本机：%v。该地址保留供诊断，但当前不能宣称公网可用。", oldURL, err), LastSyncedAt: current.LastSyncedAt, Instances: instances, Tunnels: tunnels})
}

// verifyPublicHealth asks the public URL's lightweight health endpoint and
// compares the returned local probe ID, never trusting a bare HTTP 200.
func (manager *PublicAccessManager) verifyPublicHealth(ctx context.Context, publicURL string) bool {
	base, err := normalizePublicURL(publicURL)
	if err != nil {
		return false
	}
	probe := manager.ProbeID()
	if probe == "" {
		return false
	}
	requestContext, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return false
	}
	response, err := manager.client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	var payload healthPayload
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<10))
	if err := decoder.Decode(&payload); err != nil {
		return false
	}
	return payload.App == "PhoneBridge" && payload.ProbeID == probe
}

func generateProbeID() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "unknown-probe"
	}
	return hex.EncodeToString(buffer[:])
}

type frpEnvelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// healthPayload is the response body of the lightweight GET /healthz
// endpoint used by outbound public-URL verification.
type healthPayload struct {
	App     string `json:"app"`
	Version string `json:"version"`
	ProbeID string `json:"probeId"`
}

func (manager *PublicAccessManager) fetchInstances(ctx context.Context, serviceURL string) ([]FRPInstance, error) {
	var instances []FRPInstance
	if err := manager.getJSON(ctx, serviceURL+"/api/instances", &instances); err != nil {
		return nil, err
	}
	return instances, nil
}

func (manager *PublicAccessManager) fetchInstance(ctx context.Context, serviceURL, instanceID string) ([]FRPTunnel, string, error) {
	escapedID := url.PathEscape(instanceID)
	var tunnelData struct {
		Tunnels []FRPTunnel `json:"tunnels"`
	}
	if err := manager.getJSON(ctx, serviceURL+"/api/instances/"+escapedID+"/tunnels", &tunnelData); err != nil {
		return nil, "", err
	}
	var configData struct {
		ConfigText string `json:"configText"`
	}
	if err := manager.getJSON(ctx, serviceURL+"/api/instances/"+escapedID+"/config", &configData); err != nil {
		return nil, "", err
	}
	return tunnelData.Tunnels, configData.ConfigText, nil
}

func (manager *PublicAccessManager) getJSON(ctx context.Context, address string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	response, err := manager.frpHTTPClient().Do(request)
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
		return fmt.Errorf("无法读取 88FRP 返回数据：%w", err)
	}
	if !envelope.Success {
		if envelope.Message == "" {
			envelope.Message = "未知错误"
		}
		return fmt.Errorf("88FRP 本机服务：%s", envelope.Message)
	}
	if err := json.Unmarshal(envelope.Data, target); err != nil {
		return fmt.Errorf("88FRP 返回数据格式无效：%w", err)
	}
	return nil
}

func portFromValue(value any) int {
	switch item := value.(type) {
	case float64:
		return int(item)
	case json.Number:
		value, _ := item.Int64()
		return int(value)
	case string:
		value, _ := strconv.Atoi(strings.TrimSpace(item))
		return value
	case int:
		return item
	default:
		return 0
	}
}

var frpServerAddressPattern = regexp.MustCompile(`(?mi)^\s*(?:serverAddr|server_addr)\s*=\s*(?:["']([^"']+)["']|([^\s#]+))`)

func frpServerHost(configText string) (string, error) {
	match := frpServerAddressPattern.FindStringSubmatch(configText)
	if len(match) == 0 {
		return "", fmt.Errorf("88FRP 配置中没有 serverAddr")
	}
	host := strings.TrimSpace(match[1])
	if host == "" {
		host = strings.TrimSpace(match[2])
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	if host == "" || strings.Contains(host, "/") || strings.Contains(host, "@") {
		return "", fmt.Errorf("88FRP 配置中的 serverAddr 无效")
	}
	return host, nil
}
