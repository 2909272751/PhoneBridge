package app

import (
	"strings"
	"testing"
)

// The host candidate that 88FRP forwards is rewritten to the mapped public
// port; a non-host candidate is not forwarded by the UDP proxy and is left
// untouched. Note the rewrite is driven by the candidate type, not by a port
// equality test: requiring fields[5] == localPort is what silently skipped the
// rewrite whenever the mux port differed from the (never configured) ICEPort.
func TestRewriteICECandidatePort(t *testing.T) {
	input := "v=0\r\n" +
		"a=candidate:1 1 udp 2130706431 8.163.132.151 3478 typ host\r\n" +
		"a=candidate:2 1 udp 1694498815 192.168.1.2 40000 typ srflx\r\n"
	want := "v=0\r\n" +
		"a=candidate:1 1 udp 2130706431 8.163.132.151 26188 typ host\r\n" +
		"a=candidate:2 1 udp 1694498815 192.168.1.2 40000 typ srflx\r\n"
	if got := rewriteICECandidatePort(input, 3478, 26188); got != want {
		t.Fatalf("rewritten SDP = %q, want %q", got, want)
	}
	// The mux port is authoritative even when the caller cannot supply it: the
	// host candidate must never keep a private port in the published answer.
	if got := rewriteICECandidatePort(input, 0, 26188); strings.Contains(got, " 3478 typ host") {
		t.Fatalf("private port survived an unset local port: %q", got)
	}
}

func TestHasTURNServer(t *testing.T) {
	if hasTURNServer([]ICEServerConfig{{URLs: []string{"stun:example.com:3478"}}}) {
		t.Fatal("STUN-only configuration must not be reported as TURN")
	}
	if !hasTURNServer([]ICEServerConfig{{URLs: []string{"turns:example.com:5349"}}}) {
		t.Fatal("TURN configuration was not detected")
	}
}
