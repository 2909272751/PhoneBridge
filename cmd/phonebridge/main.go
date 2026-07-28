package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"phonebridge/internal/app"
)

func main() {
	var listen string
	var demo bool
	var adbPath string
	var scrcpyPath string
	var publicURL string
	var settingsPath string
	var iceServerURLs string
	var turnUsername string
	var turnCredential string

	flag.StringVar(&listen, "listen", "127.0.0.1:8787", "HTTP 监听地址")
	flag.BoolVar(&demo, "demo", false, "启用演示设备")
	flag.StringVar(&adbPath, "adb", "", "ADB 可执行文件路径（默认使用内置组件）")
	flag.StringVar(&scrcpyPath, "scrcpy", "", "scrcpy 可执行文件路径（默认使用内置组件）")
	flag.StringVar(&publicURL, "public-url", os.Getenv("PHONEBRIDGE_PUBLIC_URL"), "对外分享链接的基础地址")
	flag.StringVar(&iceServerURLs, "ice-servers", os.Getenv("PHONEBRIDGE_ICE_SERVERS"), "逗号分隔的 STUN/TURN 地址")
	flag.StringVar(&turnUsername, "turn-username", os.Getenv("PHONEBRIDGE_TURN_USERNAME"), "TURN 用户名")
	flag.StringVar(&turnCredential, "turn-credential", os.Getenv("PHONEBRIDGE_TURN_CREDENTIAL"), "TURN 凭据")
	flag.StringVar(&settingsPath, "settings", os.Getenv("PHONEBRIDGE_SETTINGS_PATH"), "PhoneBridge local settings file")
	flag.Parse()

	var iceServers []app.ICEServerConfig
	if urls := splitURLs(iceServerURLs); len(urls) > 0 {
		iceServers = append(iceServers, app.ICEServerConfig{URLs: urls, Username: turnUsername, Credential: turnCredential})
	}

	logger := log.New(os.Stdout, "[PhoneBridge] ", log.Ldate|log.Ltime|log.Lmicroseconds)
	server := app.New(app.Config{
		ListenAddress: listen,
		DemoMode:      demo,
		ADBPath:       adbPath,
		ScrcpyPath:    scrcpyPath,
		PublicBaseURL: publicURL,
		SettingsPath:  settingsPath,
		ICEServers:    iceServers,
	}, logger)

	logger.Printf("启动 PhoneBridge，访问 http://%s", listen)
	if err := server.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "PhoneBridge 启动失败：%v\n", err)
		os.Exit(1)
	}
}

func splitURLs(value string) []string {
	result := make([]string, 0)
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
