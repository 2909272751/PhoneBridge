package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersionConsistency guards the three places the user-facing version
// appears so a version bump can never ship with them out of sync:
//  1. the backend const used by /api/status, the update check and the host
//     page's dynamic version display;
//  2. the remote-control page's static fallback badge (controller.html);
//  3. the Inno Setup AppVersion, which also drives the installer filename.
func TestVersionConsistency(t *testing.T) {
	const want = "2.1.1"

	if version != want {
		t.Errorf("server version = %q, want %q", version, want)
	}

	controller, err := os.ReadFile(filepath.Join("web", "controller.html"))
	if err != nil {
		t.Fatalf("read controller.html: %v", err)
	}
	badge := `PhoneBridge <small class="web-version">` + "v" + want + `</small>`
	if !strings.Contains(string(controller), badge) {
		t.Errorf("controller.html static version badge does not contain %q", badge)
	}

	iss, err := os.ReadFile(filepath.Join("..", "..", "installer", "PhoneBridge.iss"))
	if err != nil {
		t.Fatalf("read PhoneBridge.iss: %v", err)
	}
	directive := `#define AppVersion "` + want + `"`
	if !strings.Contains(string(iss), directive) {
		t.Errorf("PhoneBridge.iss AppVersion is not %q", want)
	}
}
