package app

import "testing"

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
}

func TestHasTURNServer(t *testing.T) {
	if hasTURNServer([]ICEServerConfig{{URLs: []string{"stun:example.com:3478"}}}) {
		t.Fatal("STUN-only configuration must not be reported as TURN")
	}
	if !hasTURNServer([]ICEServerConfig{{URLs: []string{"turns:example.com:5349"}}}) {
		t.Fatal("TURN configuration was not detected")
	}
}
