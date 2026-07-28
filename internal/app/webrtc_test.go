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
