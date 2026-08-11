package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestInstallerStaticRequirements statically guards the Inno Setup installer
// config against a regression where the post-install launch of PhoneBridge was
// skipped during silent installs (/SILENT, /VERYSILENT) and automatic upgrades.
//
// The launch lives in CurStepChanged(ssPostInstall) so it runs exactly once,
// after files are copied and the old process / ADB are stopped, for visible and
// silent installs alike; a browser is opened only for visible installs.
func TestInstallerStaticRequirements(t *testing.T) {
	iss, err := os.ReadFile(filepath.Join("..", "..", "installer", "PhoneBridge.iss"))
	if err != nil {
		t.Fatalf("read PhoneBridge.iss: %v", err)
	}
	src := string(iss)

	// skipifsilent previously suppressed the launch during silent installs.
	if strings.Contains(src, "skipifsilent") {
		t.Errorf("PhoneBridge.iss must not use skipifsilent: it prevented the silent autostart")
	}

	// Startup must happen once in CurStepChanged; a [Run] entry launching
	// PhoneBridge.exe alongside it would double-launch an instance. Match only
	// a standalone [Run] section header so [UninstallRun] is not confused.
	runSection := regexp.MustCompile(`(?m)^\[Run\]\s*$`)
	if loc := runSection.FindStringIndex(src); loc != nil && strings.Contains(src[loc[1]:], "PhoneBridge.exe") {
		t.Errorf("PhoneBridge.iss [Run] section must not launch PhoneBridge.exe (double-launch risk)")
	}

	// The single, hidden launch must be present and must run only after the
	// install finished copying files (ssPostInstall).
	for _, want := range []string{
		"CurStepChanged",
		"ssPostInstall",
		"{app}\\PhoneBridge.exe",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("PhoneBridge.iss does not contain %q", want)
		}
	}

	// The launch working directory must be {app}.
	if !strings.Contains(src, "ExpandConstant('{app}')") {
		t.Errorf("PhoneBridge.iss launch must use working directory {app}")
	}

	// Silent mode must not open a browser; visible installs may keep opening one.
	if !strings.Contains(src, "WizardSilent") {
		t.Errorf("PhoneBridge.iss must branch browser opening on WizardSilent")
	}
}
