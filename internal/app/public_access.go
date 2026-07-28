package app

import (
	"context"
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
	Source        string `json:"source"`
	ManualURL     string `json:"manualUrl"`
	FRPServiceURL string `json:"frpServiceUrl"`
	FRPInstanceID string `json:"frpInstanceId"`
	FRPTunnelName string `json:"frpTunnelName"`
	FRPScheme     string `json:"frpScheme"`
	AutoSync      bool   `json:"autoSync"`
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

type publicAccessFile struct {
	Settings PublicAccessSettings `json:"publicAccess"`
}

type PublicAccessManager struct {
	mu        sync.RWMutex
	path      string
	localPort int
	logger    *log.Logger
	client    *http.Client
	snapshot  PublicAccessSnapshot
}

func NewPublicAccessManager(settingsPath, initialURL string, localPort int, logger *log.Logger) *PublicAccessManager {
	if logger == nil {
		logger = log.Default()
	}
	manager := &PublicAccessManager{
		path:      settingsPath,
		localPort: localPort,
		logger:    logger,
		client:    &http.Client{Timeout: 6 * time.Second},
		snapshot:  PublicAccessSnapshot{Settings: defaultPublicAccessSettings(), State: "local", Message: "使用本机地址；配置公网地址后新分享链接会自动更新。"},
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
		} else if !os.IsNotExist(err) {
			return err
		}
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
	}
	return nil
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

// DiscoverUDPForward returns the public endpoint of the enabled 88FRP UDP
// proxy targeting the local ICE port. It reads only the already authenticated
// local 88FRP companion; no account secret is copied into PhoneBridge.
func (manager *PublicAccessManager) DiscoverUDPForward(ctx context.Context, localPort int) (string, int, error) {
	manager.mu.RLock()
	settings := manager.snapshot.Settings
	manager.mu.RUnlock()
	if settings.Source != publicAccess88FRP {
		return "", 0, errors.New("88FRP public sharing is not enabled")
	}
	serviceURL, err := validateLocalFRPService(settings.FRPServiceURL)
	if err != nil {
		return "", 0, err
	}
	if settings.FRPInstanceID == "" {
		return "", 0, errors.New("88FRP instance is not selected")
	}
	tunnels, configText, err := manager.fetchInstance(ctx, serviceURL, settings.FRPInstanceID)
	if err != nil {
		return "", 0, err
	}
	for _, tunnel := range tunnels {
		if tunnel.Enabled && strings.EqualFold(strings.TrimSpace(tunnel.Type), "udp") && portFromValue(tunnel.LocalPort) == localPort {
			host, hostErr := frpServerHost(configText)
			if hostErr != nil {
				return "", 0, hostErr
			}
			port := portFromValue(tunnel.RemotePort)
			if port < 1 || port > 65535 {
				return "", 0, errors.New("88FRP UDP proxy has no valid public port")
			}
			return host, port, nil
		}
	}
	return "", 0, fmt.Errorf("no enabled 88FRP UDP proxy targets local port %d", localPort)
}

func (manager *PublicAccessManager) Save(ctx context.Context, input PublicAccessSettings) (PublicAccessSnapshot, error) {
	settings := normalizePublicAccessSettings(input)
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
	data, err := json.MarshalIndent(publicAccessFile{Settings: settings}, "", "  ")
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
		manager.setSyncError(settings, nil, nil, err)
		return manager.Snapshot(), err
	}
	if settings.FRPInstanceID == "" {
		manager.setSnapshot(PublicAccessSnapshot{Settings: settings, State: "frp-needs-instance", Message: "请选择运行 PhoneBridge 的 88FRP 实例。", Instances: instances})
		return manager.Snapshot(), nil
	}
	tunnels, configText, err := manager.fetchInstance(ctx, serviceURL, settings.FRPInstanceID)
	if err != nil {
		manager.setSyncError(settings, instances, nil, err)
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

type frpEnvelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
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
	response, err := manager.client.Do(request)
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
