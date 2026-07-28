package app

import (
	"os"
	"testing"
	"time"
)

func TestADBInputShellLineQuotesArguments(t *testing.T) {
	got := adbInputShellLine("text", "can't stop")
	want := "input 'text' 'can'\\''t stop'"
	if got != want {
		t.Fatalf("adbInputShellLine() = %q, want %q", got, want)
	}
}

func TestADBControlShellDeviceSmoke(t *testing.T) {
	if os.Getenv("PHONEBRIDGE_DEVICE_SMOKE") != "1" {
		t.Skip("set PHONEBRIDGE_DEVICE_SMOKE=1 to exercise a connected Android device")
	}
	deviceID := os.Getenv("PHONEBRIDGE_DEVICE_ID")
	scrcpyPath := os.Getenv("PHONEBRIDGE_SCRCPY_PATH")
	if deviceID == "" || scrcpyPath == "" {
		t.Fatal("PHONEBRIDGE_DEVICE_ID and PHONEBRIDGE_SCRCPY_PATH are required")
	}
	controller := &adbControlShell{adbPath: bundledADBPath(scrcpyPath), deviceID: deviceID}
	defer controller.close()

	started := time.Now()
	if _, err := controller.screenSize(); err != nil {
		t.Fatal(err)
	}
	firstLookup := time.Since(started)
	started = time.Now()
	if _, err := controller.screenSize(); err != nil {
		t.Fatal(err)
	}
	cachedLookup := time.Since(started)
	started = time.Now()
	if err := controller.input("keyevent", "0"); err != nil {
		t.Fatal(err)
	}
	controlWrite := time.Since(started)
	t.Logf("screen lookup=%s cached=%s persistent control write=%s", firstLookup, cachedLookup, controlWrite)
}
