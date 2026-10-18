package app

import (
	"strings"
	"testing"
)

func TestNetworkAndTakeoverAssetsAreWired(t *testing.T) {
	html, err := embeddedWeb.ReadFile("web/controller.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := embeddedWeb.ReadFile("web/controller.js")
	if err != nil {
		t.Fatal(err)
	}
	htmlText := string(html)
	jsText := string(js)
	for _, marker := range []string{"takeover-overlay", "takeover-reclaim", "diagnostic-bitrate", "network-sparkline", "apply-network-recommendation", "copy-network-diagnostics"} {
		if !strings.Contains(htmlText, marker) {
			t.Fatalf("controller page is missing %q", marker)
		}
	}
	for _, marker := range []string{"control_taken_over", "showTakeover", "transportHint === \"frp-udp\"", "88FRP UDP 中继", "lastReconnectReason", "sessionStorage"} {
		if !strings.Contains(jsText, marker) {
			t.Fatalf("controller script is missing %q", marker)
		}
	}
}
