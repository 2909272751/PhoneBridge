package app

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type ControlEvent struct {
	Type string  `json:"type"`
	X    float64 `json:"x,omitempty"`
	Y    float64 `json:"y,omitempty"`
	Key  string  `json:"key,omitempty"`
}

func (server *Server) dispatchControl(deviceID string, event ControlEvent, start *PointerPoint) error {
	if event.Type == "pointer-down" {
		return nil
	}
	if event.Type == "pointer-up" {
		size, err := deviceScreenSize(context.Background(), server.config.ADBPath, deviceID)
		if err != nil {
			return err
		}
		endX, endY := screenPoint(size, event.X, event.Y)
		if start == nil {
			return adbInput(context.Background(), server.config.ADBPath, deviceID, "tap", strconv.Itoa(endX), strconv.Itoa(endY))
		}
		startX, startY := screenPoint(size, start.X, start.Y)
		if math.Abs(float64(endX-startX)) < 9 && math.Abs(float64(endY-startY)) < 9 {
			return adbInput(context.Background(), server.config.ADBPath, deviceID, "tap", strconv.Itoa(endX), strconv.Itoa(endY))
		}
		return adbInput(context.Background(), server.config.ADBPath, deviceID, "swipe", strconv.Itoa(startX), strconv.Itoa(startY), strconv.Itoa(endX), strconv.Itoa(endY), "220")
	}
	if event.Type == "scroll" {
		size, err := deviceScreenSize(context.Background(), server.config.ADBPath, deviceID)
		if err != nil {
			return err
		}
		middleX, middleY, distance := size.Width/2, size.Height/2, size.Height/4
		if event.Y > 0 {
			return adbInput(context.Background(), server.config.ADBPath, deviceID, "swipe", strconv.Itoa(middleX), strconv.Itoa(middleY-distance), strconv.Itoa(middleX), strconv.Itoa(middleY+distance), "180")
		}
		return adbInput(context.Background(), server.config.ADBPath, deviceID, "swipe", strconv.Itoa(middleX), strconv.Itoa(middleY+distance), strconv.Itoa(middleX), strconv.Itoa(middleY-distance), "180")
	}
	if event.Type == "system" {
		keycodes := map[string]string{"power": "26", "volume-up": "24", "volume-down": "25", "back": "4", "home": "3", "recents": "187"}
		keycode, ok := keycodes[event.Key]
		if !ok {
			return fmt.Errorf("暂不支持此系统操作")
		}
		return adbInput(context.Background(), server.config.ADBPath, deviceID, "keyevent", keycode)
	}
	if event.Type == "key" {
		keycodes := map[string]string{"Enter": "66", "Backspace": "67", "Escape": "4"}
		if keycode, ok := keycodes[event.Key]; ok {
			return adbInput(context.Background(), server.config.ADBPath, deviceID, "keyevent", keycode)
		}
		if len([]rune(event.Key)) == 1 {
			return adbInput(context.Background(), server.config.ADBPath, deviceID, "text", strings.ReplaceAll(event.Key, " ", "%s"))
		}
	}
	return nil
}

func screenPoint(size ScreenSize, x, y float64) (int, int) {
	x = math.Max(0, math.Min(1, x))
	y = math.Max(0, math.Min(1, y))
	return int(math.Round(x * float64(size.Width-1))), int(math.Round(y * float64(size.Height-1)))
}
