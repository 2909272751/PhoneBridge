package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

func TestVideoProfileSettings(t *testing.T) {
	smooth := settingsForVideoProfile(normalizeVideoProfile("smooth"))
	quality := settingsForVideoProfile(normalizeVideoProfile("quality"))
	if smooth.maxSize >= quality.maxSize || smooth.maxFPS >= quality.maxFPS || smooth.bitrate >= quality.bitrate {
		t.Fatalf("smooth profile must be lighter than quality: smooth=%+v quality=%+v", smooth, quality)
	}
	if got := normalizeVideoProfile("unknown"); got != videoProfileAuto {
		t.Fatalf("unknown profile = %q, want auto", got)
	}
}

// This is deliberately opt-in: it validates the exact phone/server/socket
// path on a USB-connected Android device without making normal unit tests
// depend on hardware.
func TestScrcpyDeviceSmoke(t *testing.T) {
	if os.Getenv("PHONEBRIDGE_DEVICE_SMOKE") != "1" {
		t.Skip("set PHONEBRIDGE_DEVICE_SMOKE=1 to exercise a connected Android device")
	}
	deviceID := os.Getenv("PHONEBRIDGE_DEVICE_ID")
	if deviceID == "" {
		t.Fatal("PHONEBRIDGE_DEVICE_ID is required")
	}
	scrcpyPath := os.Getenv("PHONEBRIDGE_SCRCPY_PATH")
	if scrcpyPath == "" {
		t.Fatal("PHONEBRIDGE_SCRCPY_PATH is required")
	}
	scrcpy := discoverScrcpy(scrcpyPath)
	if !scrcpy.Available {
		t.Fatal(scrcpy.Message)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := startScrcpyVideoStream(ctx, bundledADBPath(scrcpy.Path), deviceID, bundledScrcpyServerPath(scrcpy.Path), string(videoProfileSmooth))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.close()
	firstFrame := errors.New("first h264 frame received")
	err = stream.forward(sampleWriterFunc(func(media.Sample) error { return firstFrame }), nil)
	if !errors.Is(err, firstFrame) {
		t.Fatalf("expected first H.264 frame, got %v", err)
	}
}

type sampleWriterFunc func(media.Sample) error

func (fn sampleWriterFunc) WriteSample(sample media.Sample) error {
	return fn(sample)
}
