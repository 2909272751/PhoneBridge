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
	low := settingsForVideoProfile(normalizeVideoProfile("low"))
	standard := settingsForVideoProfile(normalizeVideoProfile("standard"))
	hd := settingsForVideoProfile(normalizeVideoProfile("hd"))
	quality := settingsForVideoProfile(normalizeVideoProfile("quality"))
	ultra := settingsForVideoProfile(normalizeVideoProfile("ultra"))
	profiles := []videoProfileSettings{smooth, low, standard, hd, quality, ultra}
	for index := 1; index < len(profiles); index++ {
		if profiles[index-1].maxSize >= profiles[index].maxSize || profiles[index-1].maxFPS > profiles[index].maxFPS || profiles[index-1].bitrate >= profiles[index].bitrate {
			t.Fatalf("profiles must increase by tier: previous=%+v current=%+v", profiles[index-1], profiles[index])
		}
	}
	auto := settingsForVideoProfile(normalizeVideoProfile("auto"))
	if auto != hd {
		t.Fatalf("auto baseline = %+v, want hd %+v", auto, hd)
	}
	if got := normalizeVideoProfile("unknown"); got != videoProfileHD {
		t.Fatalf("unknown profile = %q, want hd", got)
	}
	custom, err := customVideoProfileSettings(960, 15)
	if err != nil {
		t.Fatal(err)
	}
	if custom.maxSize != 960 || custom.maxFPS != 15 || custom.bitrate < 550_000 || custom.bitrate > 4_500_000 {
		t.Fatalf("unexpected custom profile: %+v", custom)
	}
	if _, err := customVideoProfileSettings(640, 60); err == nil {
		t.Fatal("unsupported custom video combination was accepted")
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
	stream, err := startScrcpyVideoStream(ctx, bundledADBPath(scrcpy.Path), deviceID, bundledScrcpyServerPath(scrcpy.Path), string(videoProfileSmooth), videoProfileSettings{}, true)
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
