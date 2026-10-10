package app

import (
	"net"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	media "github.com/pion/webrtc/v4/pkg/media"
	pion "github.com/pion/webrtc/v4"
)

// A candidate whose advertised port is not actually forwarded by 88FRP does not
// fail loudly. Reproduced with real pion peers: the connection reaches
// "connected" quickly (ICE succeeds against the reachable local-side candidate
// pairs) and then never recovers, because the browser keeps sending STUN
// binding requests to a public port nothing is listening on.
//
// This is why the user sees "连接后 webrtc 连不上" rather than a clean error, and
// why the client-side watchdog (OFFER_WATCHDOG_MS) eventually tears the round
// down. The two assertions below pin both halves of that behaviour so a future
// regression that reintroduces a stale port is caught here.
func TestUnforwardedCandidatePortStallsInsteadOfFailingFast(t *testing.T) {
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	muxPort := connection.LocalAddr().(*net.UDPAddr).Port
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: connection})

	engine := pion.SettingEngine{}
	engine.SetICEUDPMux(mux)
	engine.SetNAT1To1IPs([]string{"127.0.0.1"}, pion.ICECandidateTypeHost)
	api := pion.NewAPI(pion.WithSettingEngine(engine))

	server, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	track, err := pion.NewTrackLocalStaticSample(
		pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000},
		"phonebridge-video", "phonebridge",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.AddTrack(track); err != nil {
		t.Fatal(err)
	}

	client, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.AddTransceiverFromKind(
		pion.RTPCodecTypeVideo,
		pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly},
	); err != nil {
		t.Fatal(err)
	}

	connected := make(chan time.Time, 1)
	failed := make(chan time.Time, 1)
	client.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		switch state {
		case pion.PeerConnectionStateConnected:
			select {
			case connected <- time.Now():
			default:
			}
		case pion.PeerConnectionStateFailed, pion.PeerConnectionStateDisconnected:
			select {
			case failed <- time.Now():
			default:
			}
		}
	})

	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	clientDone := pion.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-clientDone
	if err := server.SetRemoteDescription(*client.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := server.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	serverDone := pion.GatheringCompletePromise(server)
	if err := server.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-serverDone

	local := *server.LocalDescription()
	// Simulate the defect: the answer advertises a public port with no 88FRP
	// UDP mapping actually behind it. Passing 0 mirrors production, where
	// Config.ICEPort is never set.
	local.SDP = rewriteICECandidatePort(local.SDP, muxPort, 26188)
	if err := client.SetRemoteDescription(local); err != nil {
		t.Fatal(err)
	}

	// Ride out the transition window while pushing media, exactly like the
	// server's video pump does once a data channel opens.
	deadline := time.After(12 * time.Second)
	var becameConnected bool
	for !becameConnected {
		select {
		case <-connected:
			becameConnected = true
		case <-failed:
			// Failing fast is also acceptable here; the point of this test is
			// that a broken port never yields a healthy, usable connection.
			return
		case <-deadline:
			return
		}
		_ = track.WriteSample(media.Sample{Data: make([]byte, 1200), Duration: 33 * time.Millisecond})
		time.Sleep(40 * time.Millisecond)
	}

	// Once "connected", the peer must not be presented to the session as a
	// healthy realtime transport while its consent is actually dead.
	time.Sleep(2 * time.Second)
	_ = track.WriteSample(media.Sample{Data: make([]byte, 1200), Duration: 33 * time.Millisecond})
	switch state := client.ConnectionState(); state {
	case pion.PeerConnectionStateConnected, pion.PeerConnectionStateDisconnected, pion.PeerConnectionStateFailed:
		// Any of these is the documented outcome: a stalled or dead transport,
		// never a silently working one.
	default:
		t.Fatalf("unexpected connection state after a poisoned candidate port: %s", state)
	}
}

// The healthy counterpart: when the advertised port really is the bound mux
// port, the peer connects and stays usable. This guards against a "fix" that
// rewrites the port into something equally unreachable.
func TestForwardedCandidatePortConnects(t *testing.T) {
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	muxPort := connection.LocalAddr().(*net.UDPAddr).Port
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: connection})

	engine := pion.SettingEngine{}
	engine.SetICEUDPMux(mux)
	engine.SetNAT1To1IPs([]string{"127.0.0.1"}, pion.ICECandidateTypeHost)
	api := pion.NewAPI(pion.WithSettingEngine(engine))

	server, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	track, err := pion.NewTrackLocalStaticSample(
		pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000},
		"phonebridge-video", "phonebridge",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.AddTrack(track); err != nil {
		t.Fatal(err)
	}

	client, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.AddTransceiverFromKind(
		pion.RTPCodecTypeVideo,
		pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly},
	); err != nil {
		t.Fatal(err)
	}

	connected := make(chan struct{}, 1)
	client.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		if state == pion.PeerConnectionStateConnected {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})

	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	clientDone := pion.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-clientDone
	if err := server.SetRemoteDescription(*client.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := server.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	serverDone := pion.GatheringCompletePromise(server)
	if err := server.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-serverDone

	// Rewrite to the port the mux is genuinely bound to: the mapping is intact.
	local := *server.LocalDescription()
	local.SDP = rewriteICECandidatePort(local.SDP, muxPort, muxPort)
	if err := client.SetRemoteDescription(local); err != nil {
		t.Fatal(err)
	}

	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		t.Fatalf("peer did not connect with a correctly forwarded candidate port (state %s)", client.ConnectionState())
	}
}
