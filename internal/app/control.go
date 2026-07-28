package app

import (
	"context"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ControlEvent struct {
	Type string  `json:"type"`
	X    float64 `json:"x,omitempty"`
	Y    float64 `json:"y,omitempty"`
	Key  string  `json:"key,omitempty"`
}

// adbControlShell keeps one device shell alive for the duration of a share.
// Starting a fresh `adb shell input ...` process for every tap adds avoidable
// process and USB round trips to every interaction. The persistent shell turns
// control events into a small stdin write while retaining the old one-shot ADB
// command as a fallback if the shell cannot be started.
type adbControlShell struct {
	adbPath  string
	deviceID string
	mu       sync.Mutex
	command  *exec.Cmd
	stdin    io.WriteCloser
	size     ScreenSize
	sizeAt   time.Time
}

func (server *Server) controlShell(deviceID string) *adbControlShell {
	server.controlMu.Lock()
	defer server.controlMu.Unlock()
	controller := server.controllers[deviceID]
	if controller == nil {
		controller = &adbControlShell{adbPath: server.config.ADBPath, deviceID: deviceID}
		server.controllers[deviceID] = controller
	}
	return controller
}

func (server *Server) closeControlShells() {
	server.controlMu.Lock()
	controllers := make([]*adbControlShell, 0, len(server.controllers))
	for _, controller := range server.controllers {
		controllers = append(controllers, controller)
	}
	server.controllers = make(map[string]*adbControlShell)
	server.controlMu.Unlock()
	for _, controller := range controllers {
		controller.close()
	}
}

func (controller *adbControlShell) screenSize() (ScreenSize, error) {
	controller.mu.Lock()
	if controller.size.Width > 0 && time.Since(controller.sizeAt) < 30*time.Second {
		size := controller.size
		controller.mu.Unlock()
		return size, nil
	}
	controller.mu.Unlock()

	size, err := deviceScreenSize(context.Background(), controller.adbPath, controller.deviceID)
	if err != nil {
		return ScreenSize{}, err
	}
	controller.mu.Lock()
	controller.size = size
	controller.sizeAt = time.Now()
	controller.mu.Unlock()
	return size, nil
}

func (controller *adbControlShell) input(arguments ...string) error {
	line := adbInputShellLine(arguments...)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if err := controller.startLocked(); err != nil {
			break
		}
		if _, err := io.WriteString(controller.stdin, line+"\n"); err == nil {
			return nil
		}
		controller.stopLocked()
	}
	// A persistent shell is an optimization, not a new point of failure.
	return adbInput(context.Background(), controller.adbPath, controller.deviceID, arguments...)
}

func (controller *adbControlShell) startLocked() error {
	if controller.command != nil && controller.stdin != nil {
		return nil
	}
	command := exec.Command(controller.adbPath, "-s", controller.deviceID, "shell")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	if err = command.Start(); err != nil {
		_ = stdin.Close()
		return err
	}
	controller.command = command
	controller.stdin = stdin
	return nil
}

func (controller *adbControlShell) close() {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.stopLocked()
}

func (controller *adbControlShell) stopLocked() {
	if controller.stdin != nil {
		_ = controller.stdin.Close()
	}
	if controller.command != nil && controller.command.Process != nil {
		_ = controller.command.Process.Kill()
		_ = controller.command.Wait()
	}
	controller.stdin = nil
	controller.command = nil
}

func adbInputShellLine(arguments ...string) string {
	quoted := make([]string, 0, len(arguments))
	for _, value := range arguments {
		quoted = append(quoted, "'"+strings.ReplaceAll(value, "'", "'\\''")+"'")
	}
	return "input " + strings.Join(quoted, " ")
}

func (server *Server) dispatchControl(deviceID string, event ControlEvent, start *PointerPoint) error {
	controller := server.controlShell(deviceID)
	if event.Type == "pointer-down" {
		return nil
	}
	if event.Type == "pointer-up" {
		size, err := controller.screenSize()
		if err != nil {
			return err
		}
		endX, endY := screenPoint(size, event.X, event.Y)
		if start == nil {
			return controller.input("tap", strconv.Itoa(endX), strconv.Itoa(endY))
		}
		startX, startY := screenPoint(size, start.X, start.Y)
		if math.Abs(float64(endX-startX)) < 9 && math.Abs(float64(endY-startY)) < 9 {
			return controller.input("tap", strconv.Itoa(endX), strconv.Itoa(endY))
		}
		return controller.input("swipe", strconv.Itoa(startX), strconv.Itoa(startY), strconv.Itoa(endX), strconv.Itoa(endY), "160")
	}
	if event.Type == "scroll" {
		size, err := controller.screenSize()
		if err != nil {
			return err
		}
		middleX, middleY, distance := size.Width/2, size.Height/2, size.Height/4
		if event.Y > 0 {
			return controller.input("swipe", strconv.Itoa(middleX), strconv.Itoa(middleY-distance), strconv.Itoa(middleX), strconv.Itoa(middleY+distance), "150")
		}
		return controller.input("swipe", strconv.Itoa(middleX), strconv.Itoa(middleY+distance), strconv.Itoa(middleX), strconv.Itoa(middleY-distance), "150")
	}
	if event.Type == "system" {
		keycodes := map[string]string{"power": "26", "volume-up": "24", "volume-down": "25", "back": "4", "home": "3", "recents": "187"}
		keycode, ok := keycodes[event.Key]
		if !ok {
			return fmt.Errorf("暂不支持此系统操作")
		}
		return controller.input("keyevent", keycode)
	}
	if event.Type == "key" {
		keycodes := map[string]string{"Enter": "66", "Backspace": "67", "Escape": "4"}
		if keycode, ok := keycodes[event.Key]; ok {
			return controller.input("keyevent", keycode)
		}
		if len([]rune(event.Key)) == 1 {
			return controller.input("text", strings.ReplaceAll(event.Key, " ", "%s"))
		}
	}
	return nil
}

func screenPoint(size ScreenSize, x, y float64) (int, int) {
	x = math.Max(0, math.Min(1, x))
	y = math.Max(0, math.Min(1, y))
	return int(math.Round(x * float64(size.Width-1))), int(math.Round(y * float64(size.Height-1)))
}
