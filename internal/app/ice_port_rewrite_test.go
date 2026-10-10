package app

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
)

// The production default: Config.ICEPort is never populated (no flag, env var
// or settings field sets it), so every caller used to pass 0. This is the
// regression that made the advertised public UDP port unusable.
func TestRewriteICECandidatePortMatchesTheBoundMuxPort(t *testing.T) {
	// Shape of the answer Pion really emits behind a UDPMux with NAT1To1IPs
	// applied: the public IP is already substituted, but the port is the mux's
	// own bound port.
	realAnswer := "v=0\r\n" +
		"a=candidate:3070051959 1 udp 2130706431 112.74.85.74 52005 typ host ufrag abcd\r\n" +
		"a=candidate:3070051959 2 udp 2130706431 112.74.85.74 52005 typ host ufrag abcd\r\n"
	got := rewriteICECandidatePort(realAnswer, 52005, 26188)
	if strings.Contains(got, " 52005 typ host") {
		t.Fatalf("private mux port survived the rewrite: %q", got)
	}
	if count := strings.Count(got, " 26188 typ host"); count != 2 {
		t.Fatalf("expected both host candidates rewritten to 26188, got %d in %q", count, got)
	}
}

// A caller that passes 0 (the historical production default) must still reach a
// consistent result rather than silently leaving the private port in place.
func TestRewriteICECandidatePortWithUnsetLocalPort(t *testing.T) {
	answer := "v=0\r\n" +
		"a=candidate:1 1 udp 2130706431 112.74.85.74 52005 typ host ufrag abcd\r\n"
	got := rewriteICECandidatePort(answer, 0, 26188)
	if !strings.Contains(got, " 26188 typ host") {
		t.Fatalf("unset local port must not block the rewrite: %q", got)
	}
	if strings.Contains(got, " 52005 ") {
		t.Fatalf("private mux port survived: %q", got)
	}
}

// The rewrite is a 1:1 NAT mapping: it must apply to the host candidate 88FRP
// forwards. Non-host candidates are not forwarded by the UDP proxy and must be
// left untouched so their own reachability is not corrupted.
func TestRewriteICECandidatePortLeavesNonHostCandidatesAlone(t *testing.T) {
	input := "v=0\r\n" +
		"a=candidate:1 1 udp 2130706431 112.74.85.74 52005 typ host ufrag abcd\r\n" +
		"a=candidate:2 1 udp 1694498815 192.168.1.2 40000 typ srflx raddr 0.0.0.0 rport 0\r\n"
	got := rewriteICECandidatePort(input, 52005, 26188)
	if !strings.Contains(got, " 26188 typ host") {
		t.Fatalf("host candidate was not rewritten: %q", got)
	}
	if !strings.Contains(got, "192.168.1.2 40000 typ srflx") {
		t.Fatalf("srflx candidate must be preserved verbatim: %q", got)
	}
}

// An SDP with no host candidate must be returned unchanged (no accidental
// mangling of unrelated lines or line endings).
func TestRewriteICECandidatePortWithoutHostCandidatesIsIdentity(t *testing.T) {
	input := "v=0\r\na=candidate:2 1 udp 1694498815 192.168.1.2 40000 typ srflx\r\n"
	if got := rewriteICECandidatePort(input, 3478, 26188); got != input {
		t.Fatalf("SDP changed without a host candidate:\n got %q\nwant %q", got, input)
	}
}

// iceLocalPort must report the socket the mux actually bound, not the config
// value that production never sets. Discovery compares this port with each
// 88FRP tunnel's localPort, so a wrong value is what makes UDP forwarding
// silently disappear.
func TestIceLocalPortReportsBoundSocket(t *testing.T) {
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	bound := connection.LocalAddr().(*net.UDPAddr).Port
	server := &Server{iceConn: connection}
	if got := server.iceLocalPort(); got != bound {
		t.Fatalf("iceLocalPort = %d, want the bound port %d", got, bound)
	}
	// With no socket bound (Config.ICEPort unset) the documented default holds.
	empty := &Server{}
	if got := empty.iceLocalPort(); got != defaultICEPort {
		t.Fatalf("iceLocalPort without a socket = %d, want %d", got, defaultICEPort)
	}
}

// End-to-end guarantee for the answer the server actually publishes: the SDP a
// browser receives must carry the 88FRP public port, never the private mux port.
// This is the defect that made ICE try to reach a port 88FRP does not forward.
func TestAnswerAdvertisesMappedPublicPortNotMuxPort(t *testing.T) {
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	muxPort := connection.LocalAddr().(*net.UDPAddr).Port
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: connection})

	engine := pion.SettingEngine{}
	engine.SetICEUDPMux(mux)
	engine.SetNAT1To1IPs([]string{"198.51.100.7"}, pion.ICECandidateTypeHost)
	api := pion.NewAPI(pion.WithSettingEngine(engine))

	pc, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	track, err := pion.NewTrackLocalStaticSample(
		pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000},
		"phonebridge-video", "phonebridge",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	done := pion.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-done

	const publicPort = 26188
	local := *pc.LocalDescription()
	local.SDP = rewriteICECandidatePort(local.SDP, muxPort, publicPort)

	hostLines := 0
	for _, line := range strings.Split(local.SDP, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "a=candidate:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[7] != "host" {
			continue
		}
		hostLines++
		if fields[5] != fmt.Sprint(publicPort) {
			t.Fatalf("published host candidate uses port %s, want the 88FRP public port %d: %s", fields[5], publicPort, line)
		}
	}
	if hostLines == 0 {
		t.Fatal("no host candidate was gathered, the assertion would be vacuous")
	}
}
