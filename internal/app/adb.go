package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ADBSnapshot struct {
	Available bool      `json:"available"`
	Message   string    `json:"message"`
	Devices   []Device  `json:"devices"`
	UpdatedAt time.Time `json:"updatedAt"`
}

var sizePattern = regexp.MustCompile(`(?:Physical|Override) size:\s*(\d+)x(\d+)`)

type ScreenSize struct {
	Width  int
	Height int
}

func deviceScreenSize(ctx context.Context, adbPath, deviceID string) (ScreenSize, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, adbPath, "-s", deviceID, "shell", "wm", "size")
	hideBackgroundProcess(command)
	output, err := command.CombinedOutput()
	if err != nil {
		return ScreenSize{}, fmt.Errorf("无法读取手机屏幕尺寸：%w", err)
	}
	matches := sizePattern.FindAllStringSubmatch(string(output), -1)
	if len(matches) == 0 {
		return ScreenSize{}, errors.New("无法识别手机屏幕尺寸")
	}
	match := matches[len(matches)-1]
	width, _ := strconv.Atoi(match[1])
	height, _ := strconv.Atoi(match[2])
	if width < 1 || height < 1 {
		return ScreenSize{}, errors.New("手机屏幕尺寸无效")
	}
	return ScreenSize{Width: width, Height: height}, nil
}

func deviceScreenshot(ctx context.Context, adbPath, deviceID string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, adbPath, "-s", deviceID, "exec-out", "screencap", "-p")
	hideBackgroundProcess(command)
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("无法获取手机画面：%w", err)
	}
	if len(output) < 128 {
		return nil, errors.New("手机未返回有效画面")
	}
	return output, nil
}

// wakeDevice is idempotent: KEYCODE_WAKEUP leaves an already-awake phone
// unchanged. It prevents a new remote viewer from receiving a black but valid
// screenshot when Android has turned the display off.
func wakeDevice(ctx context.Context, adbPath, deviceID string) error {
	return adbInput(ctx, adbPath, deviceID, "keyevent", "224")
}

// disableTouchDebugOverlays removes Android developer overlays that draw
// circles, trails, and coordinates over every injected remote touch.
func disableTouchDebugOverlays(ctx context.Context, adbPath, deviceID string) {
	for _, setting := range []string{"show_touches", "pointer_location"} {
		commandContext, cancel := context.WithTimeout(ctx, 3*time.Second)
		command := exec.CommandContext(commandContext, adbPath, "-s", deviceID, "shell", "settings", "put", "system", setting, "0")
		hideBackgroundProcess(command)
		_ = command.Run()
		cancel()
	}
}

func devicePreview(ctx context.Context, adbPath, deviceID string) ([]byte, string, error) {
	raw, err := deviceScreenshot(ctx, adbPath, deviceID)
	if err != nil {
		return nil, "", err
	}
	source, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("无法解码手机画面：%w", err)
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width < 1 || height < 1 {
		return nil, "", errors.New("手机画面尺寸无效")
	}
	const maxWidth = 540
	if width > maxWidth {
		newHeight := height * maxWidth / width
		resized := image.NewRGBA(image.Rect(0, 0, maxWidth, newHeight))
		for y := 0; y < newHeight; y++ {
			sourceY := bounds.Min.Y + y*height/newHeight
			for x := 0; x < maxWidth; x++ {
				sourceX := bounds.Min.X + x*width/maxWidth
				resized.Set(x, y, source.At(sourceX, sourceY))
			}
		}
		source = resized
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, source, &jpeg.Options{Quality: 58}); err != nil {
		return nil, "", fmt.Errorf("无法压缩手机画面：%w", err)
	}
	return encoded.Bytes(), "image/jpeg", nil
}

func adbInput(ctx context.Context, adbPath, deviceID string, input ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	arguments := append([]string{"-s", deviceID, "shell", "input"}, input...)
	command := exec.CommandContext(ctx, adbPath, arguments...)
	hideBackgroundProcess(command)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("手机未接受控制操作：%s", message)
	}
	return nil
}

type Device struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	Model        string `json:"model"`
	Product      string `json:"product,omitempty"`
	TransportID  string `json:"transportId,omitempty"`
	AndroidLabel string `json:"androidLabel"`
	Connection   string `json:"connection"`
	IsDemo       bool   `json:"isDemo"`
}

func discoverADB(ctx context.Context, adbPath string, demo bool) ADBSnapshot {
	// A cold Windows ADB server may need several seconds to initialise USB
	// drivers. Once the daemon is warm this command still returns immediately.
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, adbPath, "devices", "-l")
	hideBackgroundProcess(cmd)
	output, err := cmd.CombinedOutput()
	now := time.Now()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			message = "ADB 响应超时，请检查驱动或重启 ADB 服务"
		} else if message == "" {
			message = fmt.Sprintf("找不到或无法运行 ADB：%v", err)
		}
		return snapshotWithDemo(false, message, now, demo)
	}

	devices := parseADBDevices(string(output))
	if demo {
		devices = append(devices, demoDevice())
	}
	message := "ADB 已就绪"
	if len(devices) == 0 {
		message = "ADB 已就绪，尚未发现已授权的手机"
	}
	return ADBSnapshot{
		Available: true,
		Message:   message,
		Devices:   devices,
		UpdatedAt: now,
	}
}

func snapshotWithDemo(available bool, message string, now time.Time, demo bool) ADBSnapshot {
	devices := []Device{}
	if demo {
		devices = append(devices, demoDevice())
		message += "；当前显示演示设备"
	}
	return ADBSnapshot{
		Available: available,
		Message:   message,
		Devices:   devices,
		UpdatedAt: now,
	}
}

func demoDevice() Device {
	return Device{
		ID:           "demo-android-001",
		State:        "device",
		Model:        "PhoneBridge 演示手机",
		Product:      "demo",
		TransportID:  "demo",
		AndroidLabel: "Android 15 · 演示模式",
		Connection:   "virtual",
		IsDemo:       true,
	}
}

func parseADBDevices(output string) []Device {
	var devices []Device
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "List of devices attached") || strings.HasPrefix(line, "* daemon") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		device := Device{
			ID:           fields[0],
			State:        fields[1],
			Model:        "Android 设备",
			AndroidLabel: "Android · 版本将在建立视频通道后读取",
			Connection:   "usb",
		}
		for _, field := range fields[2:] {
			parts := strings.SplitN(field, ":", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "model":
				device.Model = strings.ReplaceAll(parts[1], "_", " ")
			case "product":
				device.Product = parts[1]
			case "transport_id":
				device.TransportID = parts[1]
			}
		}
		devices = append(devices, device)
	}
	return devices
}
