package app

import "testing"

func TestParseADBListenerPID(t *testing.T) {
	output := `
  TCP    127.0.0.1:5037         0.0.0.0:0              LISTENING       4321
  TCP    127.0.0.1:8787         0.0.0.0:0              LISTENING       9000
`
	pid, ok := parseADBListenerPID(output)
	if !ok || pid != 4321 {
		t.Fatalf("parseADBListenerPID() = %d, %v; want 4321, true", pid, ok)
	}
}

func TestADBProcessNameSafety(t *testing.T) {
	for _, name := range []string{"adb.exe", "TangoADB.exe", "nox_adb.exe"} {
		if !isADBProcessName(name) {
			t.Fatalf("%q should be recognised as an ADB process", name)
		}
	}
	for _, name := range []string{"PhoneBridge.exe", "svchost.exe", "adb"} {
		if isADBProcessName(name) {
			t.Fatalf("%q must not be force-stopped", name)
		}
	}
}
