package app

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
)

const (
	frameChunkSize = 12 * 1024
	frameInterval  = 900 * time.Millisecond
	// Keep the established compatibility timeout. A browser without a usable
	// STUN path must still have enough time to offer its normal UDP candidates.
	fastICEGatherTimeout = 12 * time.Second
)

type webRTCPeer struct {
	server         *Server
	sessionID      string
	pc             *pion.PeerConnection
	done           chan struct{}
	frameAcks      chan uint32
	videoTrack     *pion.TrackLocalStaticSample
	profileUpdates chan videoProfileUpdate
	videoOnce      sync.Once
	videoStop      context.CancelFunc
	streamMu       sync.RWMutex
	stream         *scrcpyVideoStream
	closeOnce      sync.Once
	statsMu        sync.RWMutex
	stats          webRTCPeerStats
}

// webRTCPeerStats deliberately contains no network candidates, SDP, device ID,
// or access secret. It makes a live transport failure diagnosable from the
// owner page without exposing sensitive connection details.
type webRTCPeerStats struct {
	DataChannelOpen bool   `json:"dataChannelOpen"`
	FramesCaptured  uint64 `json:"framesCaptured"`
	FramesSent      uint64 `json:"framesSent"`
	VideoPackets    uint64 `json:"videoPackets"`
	VideoDropped    uint64 `json:"videoDropped"`
	VideoWidth      int    `json:"videoWidth,omitempty"`
	VideoHeight     int    `json:"videoHeight,omitempty"`
	VideoState      string `json:"videoState,omitempty"`
	SendErrors      uint64 `json:"sendErrors"`
	LastError       string `json:"lastError,omitempty"`
}

func (peer *webRTCPeer) snapshotStats() webRTCPeerStats {
	peer.statsMu.RLock()
	defer peer.statsMu.RUnlock()
	return peer.stats
}

func (peer *webRTCPeer) recordError(err error) {
	if err == nil {
		return
	}
	peer.statsMu.Lock()
	peer.stats.SendErrors++
	peer.stats.LastError = err.Error()
	peer.statsMu.Unlock()
}

type webRTCOfferRequest struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

func (server *Server) handleStreamProfile(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Profile string `json:"profile"`
		MaxSize int    `json:"maxSize"`
		MaxFPS  int    `json:"maxFps"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "传输策略请求无效")
		return
	}
	profile := string(normalizeVideoProfile(input.Profile))
	custom := videoProfileSettings{}
	if profile == string(videoProfileCustom) {
		var err error
		custom, err = customVideoProfileSettings(input.MaxSize, input.MaxFPS)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "自定义分辨率或帧率无效")
			return
		}
	}
	server.mu.Lock()
	session, ok := server.sessionByTokenLocked(request.PathValue("token"), time.Now())
	if !ok || session.ViewerState != "connected" {
		server.mu.Unlock()
		writeError(writer, http.StatusGone, "会话尚未连接或已经结束")
		return
	}
	if !viewerOwnsSession(request, session) {
		server.mu.Unlock()
		writeError(writer, http.StatusConflict, "控制已被新的连接接管")
		return
	}
	previousProfile, previousSize, previousFPS := session.StreamProfile, session.StreamMaxSize, session.StreamMaxFPS
	session.StreamProfile = profile
	session.StreamMaxSize, session.StreamMaxFPS = custom.maxSize, custom.maxFPS
	server.persistSessionsLocked()
	peer := server.webrtcPeers[session.ID]
	response := publicGuestSession(*session, true)
	server.mu.Unlock()
	if peer != nil && (previousProfile != profile || previousSize != custom.maxSize || previousFPS != custom.maxFPS) {
		peer.setVideoProfile(profile, custom)
	}
	writeJSON(writer, http.StatusOK, response)
}

func (server *Server) handleWebRTCConfig(writer http.ResponseWriter, request *http.Request) {
	session, ok := server.sessionByToken(request.PathValue("token"))
	if !ok {
		writeError(writer, http.StatusGone, "分享链接已失效")
		return
	}
	if session.ViewerState != "connected" {
		writeError(writer, http.StatusForbidden, "请先验证访问码并加入会话")
		return
	}
	if !viewerOwnsSession(request, &session) {
		writeError(writer, http.StatusConflict, "控制已被新的连接接管")
		return
	}
	transportHint := "direct"
	if server.icePublicPort != 0 {
		transportHint = "frp-udp"
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"iceServers":     server.config.ICEServers,
		"turnConfigured": hasTURNServer(server.config.ICEServers),
		"transport":      "webrtc-datachannel",
		"transportHint":  transportHint,
		"directProbe":    len(server.config.ICEServers) > 0,
	})
}

func hasTURNServer(servers []ICEServerConfig) bool {
	for _, server := range servers {
		for _, value := range server.URLs {
			value = strings.ToLower(strings.TrimSpace(value))
			if strings.HasPrefix(value, "turn:") || strings.HasPrefix(value, "turns:") {
				return true
			}
		}
	}
	return false
}

func (server *Server) handleWebRTCOffer(writer http.ResponseWriter, request *http.Request) {
	var input webRTCOfferRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "WebRTC 协商请求无效")
		return
	}
	if input.Type != "offer" || input.SDP == "" {
		writeError(writer, http.StatusBadRequest, "仅接受包含 SDP 的 WebRTC offer")
		return
	}

	server.mu.Lock()
	session, ok := server.sessionByTokenLocked(request.PathValue("token"), time.Now())
	if !ok || session.ViewerState != "connected" {
		server.mu.Unlock()
		writeError(writer, http.StatusGone, "会话尚未连接或已结束")
		return
	}
	if !viewerOwnsSession(request, session) {
		server.mu.Unlock()
		writeError(writer, http.StatusConflict, "控制已被新的连接接管")
		return
	}
	oldPeer := server.webrtcPeers[session.ID]
	delete(server.webrtcPeers, session.ID)
	session.ConnectionMode = "negotiating-webrtc"
	sessionID := session.ID
	viewerToken := session.ViewerToken
	server.mu.Unlock()
	if oldPeer != nil {
		oldPeer.close()
	}

	peer, answer, err := server.answerWebRTCOffer(request.Context(), sessionID, input)
	if err != nil {
		server.setWebRTCState(sessionID, "fallback-screen")
		writeError(writer, http.StatusBadGateway, fmt.Sprintf("WebRTC 协商失败：%v", err))
		return
	}

	server.mu.Lock()
	current := server.sessions[sessionID]
	if current == nil || current.State == "stopped" || current.State == "expired" || current.ViewerState != "connected" || current.ViewerToken != viewerToken {
		server.mu.Unlock()
		peer.close()
		writeError(writer, http.StatusGone, "会话已结束")
		return
	}
	server.webrtcPeers[sessionID] = peer
	server.mu.Unlock()
	writeJSON(writer, http.StatusOK, answer)
}

func (server *Server) answerWebRTCOffer(ctx context.Context, sessionID string, input webRTCOfferRequest) (*webRTCPeer, pion.SessionDescription, error) {
	iceServers := make([]pion.ICEServer, 0, len(server.config.ICEServers))
	for _, serverConfig := range server.config.ICEServers {
		if len(serverConfig.URLs) == 0 {
			continue
		}
		iceServers = append(iceServers, pion.ICEServer{
			URLs:       serverConfig.URLs,
			Username:   serverConfig.Username,
			Credential: serverConfig.Credential,
		})
	}
	api := server.webRTCAPI
	if api == nil {
		api = pion.NewAPI()
	}
	pc, err := api.NewPeerConnection(pion.Configuration{ICEServers: iceServers})
	if err != nil {
		return nil, pion.SessionDescription{}, err
	}
	videoTrack, err := pion.NewTrackLocalStaticSample(
		pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000},
		"phonebridge-video", "phonebridge",
	)
	if err != nil {
		_ = pc.Close()
		return nil, pion.SessionDescription{}, fmt.Errorf("创建 WebRTC 视频轨道失败：%w", err)
	}
	peer := &webRTCPeer{server: server, sessionID: sessionID, pc: pc, done: make(chan struct{}), frameAcks: make(chan uint32, 1), profileUpdates: make(chan videoProfileUpdate, 1), videoTrack: videoTrack}
	videoSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		peer.close()
		return nil, pion.SessionDescription{}, fmt.Errorf("添加 WebRTC 视频轨道失败：%w", err)
	}
	go func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, readErr := videoSender.Read(buffer); readErr != nil {
				return
			}
		}
	}()
	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		switch state {
		case pion.PeerConnectionStateConnected:
			server.setWebRTCState(sessionID, "webrtc")
			peer.startVideoPump()
		case pion.PeerConnectionStateDisconnected:
			server.setWebRTCState(sessionID, "reconnecting")
		case pion.PeerConnectionStateFailed, pion.PeerConnectionStateClosed:
			server.setWebRTCState(sessionID, "fallback-screen")
		}
	})
	pc.OnDataChannel(func(channel *pion.DataChannel) {
		if channel.Label() != "phonebridge" {
			return
		}
		channel.OnMessage(func(message pion.DataChannelMessage) {
			if !message.IsString {
				return
			}
			var envelope struct {
				Kind    string `json:"kind"`
				FrameID uint32 `json:"id"`
				ControlEvent
			}
			if json.Unmarshal(message.Data, &envelope) != nil {
				return
			}
			if envelope.Kind == "control" {
				server.handlePeerControl(sessionID, envelope.ControlEvent)
				return
			}
			if envelope.Kind == "frame-ack" {
				select {
				case peer.frameAcks <- envelope.FrameID:
				default:
				}
			}
		})
		channel.OnOpen(func() {
			peer.statsMu.Lock()
			peer.stats.DataChannelOpen = true
			peer.statsMu.Unlock()
			server.setWebRTCState(sessionID, "webrtc")
			peer.startVideoPump()
		})
		channel.OnError(peer.recordError)
	})

	offer := pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: input.SDP}
	if err = pc.SetRemoteDescription(offer); err != nil {
		peer.close()
		return nil, pion.SessionDescription{}, err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		peer.close()
		return nil, pion.SessionDescription{}, err
	}
	gatheringComplete := pion.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		peer.close()
		return nil, pion.SessionDescription{}, err
	}
	select {
	case <-gatheringComplete:
	case <-ctx.Done():
		peer.close()
		return nil, pion.SessionDescription{}, ctx.Err()
	case <-time.After(fastICEGatherTimeout):
		peer.close()
		return nil, pion.SessionDescription{}, errors.New("ICE 候选收集超时")
	}
	if pc.LocalDescription() == nil {
		peer.close()
		return nil, pion.SessionDescription{}, errors.New("未生成 WebRTC answer")
	}
	local := *pc.LocalDescription()
	if server.icePublicPort != 0 {
		local.SDP = rewriteICECandidatePort(local.SDP, server.config.ICEPort, server.icePublicPort)
	}
	return peer, local, nil
}

// 88FRP maps a public UDP port to a fixed local port. Pion correctly
// advertises the public IP through NAT1To1IPs, while this final SDP rewrite
// substitutes the mapped public port before the browser receives the answer.
func rewriteICECandidatePort(sdp string, localPort, publicPort int) string {
	if localPort == 0 {
		localPort = 3478
	}
	lines := strings.Split(sdp, "\n")
	for index, line := range lines {
		trimmed := strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(trimmed, "a=candidate:") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 8 || fields[6] != "typ" || fields[7] != "host" || fields[5] != strconv.Itoa(localPort) {
			continue
		}
		fields[5] = strconv.Itoa(publicPort)
		ending := ""
		if strings.HasSuffix(line, "\r") {
			ending = "\r"
		}
		lines[index] = strings.Join(fields, " ") + ending
	}
	return strings.Join(lines, "\n")
}

func (peer *webRTCPeer) close() {
	peer.closeOnce.Do(func() {
		if peer.videoStop != nil {
			peer.videoStop()
		}
		close(peer.done)
		_ = peer.pc.Close()
	})
}

func (peer *webRTCPeer) setStream(stream *scrcpyVideoStream) {
	peer.streamMu.Lock()
	peer.stream = stream
	peer.streamMu.Unlock()
}

func (peer *webRTCPeer) clearStream(stream *scrcpyVideoStream) {
	peer.streamMu.Lock()
	if peer.stream == stream {
		peer.stream = nil
	}
	peer.streamMu.Unlock()
}

func (peer *webRTCPeer) sendNativeControl(event ControlEvent) error {
	peer.streamMu.RLock()
	stream := peer.stream
	peer.streamMu.RUnlock()
	if stream == nil {
		return errors.New("scrcpy native control stream is not ready")
	}
	return stream.sendControl(event)
}

func (peer *webRTCPeer) startVideoPump() {
	peer.videoOnce.Do(func() {
		go func() {
			peer.setVideoState("starting")
			parent, cancel := context.WithCancel(context.Background())
			peer.videoStop = cancel
			defer cancel()
			deviceID, profile, custom, available := peer.server.connectedDeviceID(peer.sessionID)
			if !available {
				peer.recordError(errors.New("启动视频时会话或设备已不可用"))
				peer.setVideoState("unavailable")
				return
			}
			for {
				peer.setVideoState("starting-scrcpy")
				streamContext, stopStream := context.WithCancel(parent)
				stream, err := startScrcpyVideoStream(streamContext, peer.server.config.ADBPath, deviceID, bundledScrcpyServerPath(peer.server.scrcpy.Path), profile, custom, peer.server.sessionAllowsControl(peer.sessionID))
				if err != nil {
					stopStream()
					peer.recordError(fmt.Errorf("启动 scrcpy 视频流失败：%w", err))
					peer.setVideoState("failed")
					peer.server.setWebRTCState(peer.sessionID, "fallback-screen")
					return
				}
				peer.setStream(stream)
				peer.setVideoState("receiving-h264")
				writer := newLatestSampleWriter(peer.videoTrack, func() {
					peer.statsMu.Lock()
					peer.stats.VideoDropped++
					peer.statsMu.Unlock()
				})
				forwardDone := make(chan error, 1)
				go func() {
					forwardDone <- stream.forward(writer, func(width, height int) {
						peer.statsMu.Lock()
						peer.stats.VideoWidth = width
						peer.stats.VideoHeight = height
						peer.stats.VideoPackets++
						peer.statsMu.Unlock()
					})
				}()

				select {
				case next := <-peer.profileUpdates:
					profile, custom = next.profile, next.custom
					peer.setVideoState("switching-profile")
					peer.clearStream(stream)
					stream.close()
					stopStream()
					<-forwardDone
					writer.Close()
					continue
				case err = <-forwardDone:
					peer.clearStream(stream)
					stream.close()
					stopStream()
					writer.Close()
				case <-parent.Done():
					peer.clearStream(stream)
					stream.close()
					stopStream()
					<-forwardDone
					writer.Close()
					return
				}
				if err != nil && !errors.Is(err, io.EOF) && parent.Err() == nil {
					peer.recordError(fmt.Errorf("scrcpy 视频流中断：%w", err))
					peer.setVideoState("failed")
					peer.server.setWebRTCState(peer.sessionID, "fallback-screen")
				}
				return
			}
		}()
	})
}

type videoProfileUpdate struct {
	profile string
	custom  videoProfileSettings
}

func (peer *webRTCPeer) setVideoProfile(profile string, custom videoProfileSettings) {
	profile = string(normalizeVideoProfile(profile))
	update := videoProfileUpdate{profile: profile, custom: custom}
	select {
	case peer.profileUpdates <- update:
	default:
		select {
		case <-peer.profileUpdates:
		default:
		}
		select {
		case peer.profileUpdates <- update:
		default:
		}
	}
}

func (peer *webRTCPeer) setVideoState(state string) {
	peer.statsMu.Lock()
	peer.stats.VideoState = state
	peer.statsMu.Unlock()
}

func (peer *webRTCPeer) startFramePump(channel *pion.DataChannel) {
	go func() {
		ticker := time.NewTicker(frameInterval)
		defer ticker.Stop()
		var frameID uint32
		captureAndSend := func() bool {
			if channel.BufferedAmount() > 512*1024 {
				return true
			}
			deviceID, _, _, available := peer.server.connectedDeviceID(peer.sessionID)
			if !available {
				return false
			}
			frame, mimeType, err := devicePreview(context.Background(), peer.server.config.ADBPath, deviceID)
			if err != nil {
				peer.recordError(fmt.Errorf("capture preview: %w", err))
				return true
			}
			peer.statsMu.Lock()
			peer.stats.FramesCaptured++
			peer.statsMu.Unlock()
			frameID++
			if err := sendWebRTCFrame(channel, frameID, frame, mimeType); err != nil {
				peer.recordError(fmt.Errorf("send preview: %w", err))
				return false
			}
			if !peer.waitForFrameAck(frameID) {
				return false
			}
			peer.statsMu.Lock()
			peer.stats.FramesSent++
			peer.statsMu.Unlock()
			return true
		}

		if !captureAndSend() {
			return
		}
		for {
			select {
			case <-peer.done:
				return
			case <-ticker.C:
			}
			if !captureAndSend() {
				return
			}
		}
	}()
}

// waitForFrameAck prevents a newly captured frame from replacing an older
// frame while a browser is still converting received binary chunks. This is
// intentionally conservative for the screenshot fallback; the planned scrcpy
// media track will use RTP timing rather than per-frame acknowledgements.
func (peer *webRTCPeer) waitForFrameAck(frameID uint32) bool {
	timeout := time.NewTimer(4 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-peer.done:
			return false
		case ackID := <-peer.frameAcks:
			if ackID == frameID {
				return true
			}
		case <-timeout.C:
			peer.recordError(errors.New("browser frame acknowledgement timed out"))
			return false
		}
	}
}

func sendWebRTCFrame(channel *pion.DataChannel, frameID uint32, frame []byte, mimeType string) error {
	chunks := (len(frame) + frameChunkSize - 1) / frameChunkSize
	if chunks == 0 || chunks > 65535 {
		return errors.New("手机画面帧大小无效")
	}
	header, _ := json.Marshal(map[string]any{"kind": "frame", "id": frameID, "chunks": chunks, "mime": mimeType})
	if err := channel.SendText(string(header)); err != nil {
		return err
	}
	for index := 0; index < chunks; index++ {
		start := index * frameChunkSize
		end := start + frameChunkSize
		if end > len(frame) {
			end = len(frame)
		}
		packet := make([]byte, 8+end-start)
		binary.BigEndian.PutUint32(packet[0:4], frameID)
		binary.BigEndian.PutUint16(packet[4:6], uint16(index))
		binary.BigEndian.PutUint16(packet[6:8], uint16(chunks))
		copy(packet[8:], frame[start:end])
		if err := channel.Send(packet); err != nil {
			return err
		}
	}
	return nil
}

func (server *Server) connectedDeviceID(sessionID string) (string, string, videoProfileSettings, bool) {
	server.mu.RLock()
	defer server.mu.RUnlock()
	session := server.sessions[sessionID]
	if session == nil || session.IsDemo || session.State == "stopped" || session.State == "expired" || session.ViewerState != "connected" {
		return "", "", videoProfileSettings{}, false
	}
	custom, _ := customVideoProfileSettings(session.StreamMaxSize, session.StreamMaxFPS)
	return session.DeviceID, session.StreamProfile, custom, true
}

func (server *Server) sessionAllowsControl(sessionID string) bool {
	server.mu.RLock()
	defer server.mu.RUnlock()
	session := server.sessions[sessionID]
	return session != nil && !session.IsDemo && session.State != "stopped" && session.State != "expired" && session.ViewerState == "connected" && session.Mode == "control"
}

func (server *Server) setWebRTCState(sessionID, state string) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if session := server.sessions[sessionID]; session != nil && session.State != "stopped" && session.State != "expired" {
		if session.ConnectionMode == state {
			return
		}
		session.ConnectionMode = state
		server.persistSessionsLocked()
	}
}

func (server *Server) handlePeerControl(sessionID string, event ControlEvent) {
	if event.Type == "" {
		return
	}
	server.mu.Lock()
	session := server.sessions[sessionID]
	if session == nil || session.State == "stopped" || session.State == "expired" || session.ViewerState != "connected" || session.Mode != "control" {
		server.mu.Unlock()
		return
	}
	now := time.Now()
	session.LastEventAt = &now
	var start *PointerPoint
	if event.Type == "pointer-down" {
		session.pointerStart = &PointerPoint{X: event.X, Y: event.Y}
	}
	if event.Type == "pointer-up" {
		start = session.pointerStart
		session.pointerStart = nil
	}
	deviceID, demo := session.DeviceID, session.IsDemo
	peer := server.webrtcPeers[sessionID]
	server.mu.Unlock()
	if !demo {
		if peer != nil && peer.sendNativeControl(event) == nil {
			return
		}
		_ = server.dispatchControl(deviceID, event, start)
	}
}
