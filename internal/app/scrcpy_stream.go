package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	scrcpyServerVersion  = "4.1"
	scrcpyVideoCodecH264 = 0x68323634
	scrcpySessionPacket  = uint64(1) << 63
	scrcpyConfigPacket   = uint64(1) << 62
	scrcpyKeyFramePacket = uint64(1) << 61
	scrcpyPTSValueMask   = (uint64(1) << 61) - 1
	scrcpyMaxPacketSize  = 8 << 20
)

type videoProfile string

const (
	videoProfileAuto     videoProfile = "auto"
	videoProfileSmooth   videoProfile = "smooth"
	videoProfileLow      videoProfile = "low"
	videoProfileStandard videoProfile = "standard"
	videoProfileHD       videoProfile = "hd"
	videoProfileQuality  videoProfile = "quality"
	videoProfileUltra    videoProfile = "ultra"
	videoProfileCustom   videoProfile = "custom"
)

type videoProfileSettings struct {
	bitrate int
	maxSize int
	maxFPS  int
}

func normalizeVideoProfile(value string) videoProfile {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(videoProfileSmooth):
		return videoProfileSmooth
	case string(videoProfileLow):
		return videoProfileLow
	case string(videoProfileStandard):
		return videoProfileStandard
	case string(videoProfileHD):
		return videoProfileHD
	case string(videoProfileQuality):
		return videoProfileQuality
	case string(videoProfileUltra):
		return videoProfileUltra
	case string(videoProfileCustom):
		return videoProfileCustom
	}
	// There is no adaptive profile in v1.0.0: unsupported or missing values
	// deliberately resolve to the fixed 720p/24fps default.
	return videoProfileHD
}

func settingsForVideoProfile(profile videoProfile) videoProfileSettings {
	// Auto deliberately starts at 720p rather than a conservative middle tier:
	// WebRTC can shed stale frames, while users notice soft text immediately.
	// The browser controller only steps down after sustained congestion.
	if profile == videoProfileUltra {
		return videoProfileSettings{bitrate: 4_500_000, maxSize: 1080, maxFPS: 30}
	}
	if profile == videoProfileQuality {
		return videoProfileSettings{bitrate: 3_200_000, maxSize: 960, maxFPS: 24}
	}
	if profile == videoProfileHD || profile == videoProfileAuto {
		return videoProfileSettings{bitrate: 2_100_000, maxSize: 720, maxFPS: 24}
	}
	if profile == videoProfileStandard {
		return videoProfileSettings{bitrate: 1_250_000, maxSize: 540, maxFPS: 18}
	}
	if profile == videoProfileLow {
		return videoProfileSettings{bitrate: 900_000, maxSize: 480, maxFPS: 15}
	}
	if profile == videoProfileSmooth {
		return videoProfileSettings{bitrate: 550_000, maxSize: 360, maxFPS: 10}
	}
	return settingsForVideoProfile(videoProfileAuto)
}

func customVideoProfileSettings(maxSize, maxFPS int) (videoProfileSettings, error) {
	validSizes := map[int]bool{360: true, 480: true, 540: true, 720: true, 960: true, 1080: true}
	validFPS := map[int]bool{10: true, 15: true, 18: true, 24: true, 30: true}
	if !validSizes[maxSize] || !validFPS[maxFPS] {
		return videoProfileSettings{}, errors.New("custom video settings must use a supported resolution and frame rate")
	}
	// Scale bitrate to the selected pixel rate, then cap it to the highest
	// built-in tier so arbitrary combinations stay safe for public sharing.
	bitrate := int(float64(maxSize*maxSize*maxFPS) * 0.17)
	if bitrate < 550_000 {
		bitrate = 550_000
	}
	if bitrate > 4_500_000 {
		bitrate = 4_500_000
	}
	return videoProfileSettings{bitrate: bitrate, maxSize: maxSize, maxFPS: maxFPS}, nil
}

// scrcpyVideoStream reads the bundled scrcpy server protocol directly. The
// device encodes H.264 with MediaCodec; no desktop window, screenshot loop or
// H.264 decoder is inserted on the host path.
type scrcpyVideoStream struct {
	adbPath   string
	deviceID  string
	server    string
	profile   videoProfileSettings
	port      int
	command   *exec.Cmd
	output    bytes.Buffer
	done      chan struct{}
	waitMu    sync.RWMutex
	waitErr   error
	conn      net.Conn
	control   net.Conn
	controlMu sync.Mutex
	sizeMu    sync.RWMutex
	width     int
	height    int
	cancel    context.CancelFunc
}

func startScrcpyVideoStream(parent context.Context, adbPath, deviceID, serverPath string, requestedProfile string, customProfile videoProfileSettings, enableControl bool) (*scrcpyVideoStream, error) {
	if strings.TrimSpace(serverPath) == "" {
		return nil, errors.New("未找到内置 scrcpy 组件")
	}
	profileType := normalizeVideoProfile(requestedProfile)
	profile := settingsForVideoProfile(profileType)
	if profileType == videoProfileCustom {
		if customProfile.maxSize == 0 || customProfile.maxFPS == 0 || customProfile.bitrate == 0 {
			return nil, errors.New("custom video profile is incomplete")
		}
		profile = customProfile
	}
	ctx, cancel := context.WithCancel(parent)
	stream := &scrcpyVideoStream{adbPath: adbPath, deviceID: deviceID, server: serverPath, profile: profile, cancel: cancel}
	cleanup := func(err error) (*scrcpyVideoStream, error) {
		stream.close()
		return nil, err
	}

	const remoteServer = "/data/local/tmp/phonebridge-scrcpy-server-4.1.jar"
	push := exec.CommandContext(ctx, adbPath, "-s", deviceID, "push", serverPath, remoteServer)
	if output, err := push.CombinedOutput(); err != nil {
		return cleanup(fmt.Errorf("无法准备内置 scrcpy 服务：%s", commandMessage(err, output)))
	}

	var randomID [4]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return cleanup(err)
	}
	scid := binary.BigEndian.Uint32(randomID[:]) & 0x7fffffff
	socketName := fmt.Sprintf("scrcpy_%08x", scid)
	forward := exec.CommandContext(ctx, adbPath, "-s", deviceID, "forward", "tcp:0", "localabstract:"+socketName)
	output, err := forward.CombinedOutput()
	if err != nil {
		return cleanup(fmt.Errorf("无法建立 scrcpy 本机视频通道：%s", commandMessage(err, output)))
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || port < 1 || port > 65535 {
		return cleanup(errors.New("scrcpy 未返回有效的视频通道端口"))
	}
	stream.port = port

	args := []string{
		"-s", deviceID, "shell", "CLASSPATH=" + remoteServer,
		"app_process", "/", "com.genymobile.scrcpy.Server", scrcpyServerVersion,
		fmt.Sprintf("scid=%08x", scid), "log_level=warn",
		"audio=false", fmt.Sprintf("control=%t", enableControl), "tunnel_forward=true",
		"send_device_meta=false",
		"video_codec=h264", fmt.Sprintf("video_bit_rate=%d", profile.bitrate),
		fmt.Sprintf("max_size=%d", profile.maxSize), fmt.Sprintf("max_fps=%d", profile.maxFPS),
		// A short GOP limits how long a receiver must wait to recover after an
		// intentionally dropped stale frame.
		"video_codec_options=i-frame-interval:float=1",
		"cleanup=true",
	}
	stream.command = exec.CommandContext(ctx, adbPath, args...)
	stream.command.Stdout = &stream.output
	stream.command.Stderr = &stream.output
	if err := stream.command.Start(); err != nil {
		return cleanup(fmt.Errorf("无法启动内置 scrcpy 视频服务：%w", err))
	}
	stream.done = make(chan struct{})
	go func() {
		err := stream.command.Wait()
		stream.waitMu.Lock()
		stream.waitErr = err
		stream.waitMu.Unlock()
		close(stream.done)
	}()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 800*time.Millisecond)
		if err == nil {
			// adb forward accepts a local TCP connection before the device-side
			// abstract socket is listening. scrcpy deliberately writes this byte
			// on forward connections so clients can wait for the real server,
			// instead of treating an empty forwarding proxy as a video stream.
			_ = connection.SetReadDeadline(time.Now().Add(8 * time.Second))
			var ready [1]byte
			_, readyErr := io.ReadFull(connection, ready[:])
			_ = connection.SetReadDeadline(time.Time{})
			if readyErr != nil {
				_ = connection.Close()
				// adb forward may accept a TCP connection before the Android
				// abstract socket has started listening. scrcpy's own client
				// treats that EOF as a transient race and retries (rather than
				// failing the whole video session on the first attempt).
				if errors.Is(readyErr, io.EOF) || errors.Is(readyErr, io.ErrUnexpectedEOF) {
					select {
					case <-ctx.Done():
						return cleanup(ctx.Err())
					case <-time.After(100 * time.Millisecond):
						continue
					}
				}
				return cleanup(fmt.Errorf("waiting for scrcpy video service readiness: %w%s", readyErr, stream.exitDetail()))
			}
			if ready[0] != 0 {
				_ = connection.Close()
				return cleanup(fmt.Errorf("unexpected scrcpy video service readiness byte: 0x%02x", ready[0]))
			}
			stream.conn = connection
			if enableControl {
				control, controlErr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
				if controlErr != nil {
					return cleanup(fmt.Errorf("connect scrcpy control socket: %w", controlErr))
				}
				if tcp, ok := control.(*net.TCPConn); ok {
					_ = tcp.SetNoDelay(true)
				}
				stream.control = control
			}
			return stream, nil
		}
		select {
		case <-ctx.Done():
			return cleanup(ctx.Err())
		case <-time.After(120 * time.Millisecond):
		}
	}
	return cleanup(errors.New("等待 scrcpy 视频通道超时"))
}

func (stream *scrcpyVideoStream) exitDetail() string {
	if stream.done == nil {
		return ""
	}
	select {
	case <-stream.done:
		stream.waitMu.RLock()
		err := stream.waitErr
		stream.waitMu.RUnlock()
		message := strings.TrimSpace(stream.output.String())
		if message != "" {
			return ": " + message
		}
		if err != nil {
			return ": " + err.Error()
		}
	default:
	}
	return ""
}

func (stream *scrcpyVideoStream) close() {
	if stream.cancel != nil {
		stream.cancel()
	}
	if stream.conn != nil {
		_ = stream.conn.Close()
		stream.conn = nil
	}
	stream.controlMu.Lock()
	if stream.control != nil {
		_ = stream.control.Close()
		stream.control = nil
	}
	stream.controlMu.Unlock()
	if stream.port != 0 && stream.adbPath != "" && stream.deviceID != "" {
		_ = exec.Command(stream.adbPath, "-s", stream.deviceID, "forward", "--remove", "tcp:"+strconv.Itoa(stream.port)).Run()
		stream.port = 0
	}
}

func (stream *scrcpyVideoStream) forward(track sampleWriter, onFrame func(width, height int)) error {
	if stream.conn == nil {
		return errors.New("scrcpy 视频通道尚未连接")
	}
	reader := bufio.NewReaderSize(stream.conn, 256*1024)
	codec, err := readUint32(reader)
	if err != nil {
		return fmt.Errorf("无法读取 scrcpy 视频编码：%w", err)
	}
	if codec != scrcpyVideoCodecH264 {
		return fmt.Errorf("内置 scrcpy 返回了不受支持的视频编码：%08x", codec)
	}

	// With send_stream_meta enabled, scrcpy sends a 12-byte session packet after
	// the codec id: flags, width, height. It is not merely width and height.
	// The same packet may appear again when Android rotates or the encoder
	// resizes, so handle it again in the frame loop below.
	sessionFlags, err := readUint32(reader)
	if err != nil {
		return fmt.Errorf("unable to read scrcpy video session flags: %w", err)
	}
	if sessionFlags&(uint32(1)<<31) == 0 {
		return fmt.Errorf("scrcpy video stream is missing initial session metadata")
	}
	widthValue, err := readUint32(reader)
	if err != nil {
		return fmt.Errorf("unable to read scrcpy video width: %w", err)
	}
	heightValue, err := readUint32(reader)
	if err != nil {
		return fmt.Errorf("unable to read scrcpy video height: %w", err)
	}
	width, height := int(widthValue), int(heightValue)
	if width < 1 || height < 1 {
		return fmt.Errorf("scrcpy returned invalid video dimensions: %dx%d", width, height)
	}
	stream.setSize(width, height)
	if onFrame != nil {
		onFrame(width, height)
	}

	var config []byte
	var lastPTS uint64
	for {
		// Every encoded access unit has a 64-bit PTS/flags followed by its
		// payload size. This is scrcpy's default send_frame_meta=true protocol.
		header, err := readUint64(reader)
		if err != nil {
			return err
		}
		size, err := readUint32(reader)
		if err != nil {
			return err
		}
		if header&scrcpySessionPacket != 0 {
			width, height = int(uint32(header)), int(size)
			if width < 1 || height < 1 {
				return fmt.Errorf("scrcpy returned invalid resized dimensions: %dx%d", width, height)
			}
			stream.setSize(width, height)
			if onFrame != nil {
				onFrame(width, height)
			}
			continue
		}
		if size > scrcpyMaxPacketSize {
			return fmt.Errorf("scrcpy 视频包大小无效：%d", size)
		}
		if size == 0 {
			continue
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return err
		}
		if header&scrcpyConfigPacket != 0 {
			config = append(config[:0], payload...)
			continue
		}

		pts := header & scrcpyPTSValueMask
		duration := time.Second / time.Duration(stream.profile.maxFPS)
		if lastPTS != 0 && pts > lastPTS {
			candidate := time.Duration(pts-lastPTS) * time.Microsecond
			if candidate > 0 && candidate < time.Second {
				duration = candidate
			}
		}
		lastPTS = pts
		frame := payload
		if header&scrcpyKeyFramePacket != 0 && len(config) > 0 {
			frame = append(append(make([]byte, 0, len(config)+len(payload)), config...), payload...)
		}
		if err := track.WriteSample(media.Sample{Data: frame, Duration: duration}); err != nil {
			return err
		}
	}
}

func (stream *scrcpyVideoStream) setSize(width, height int) {
	stream.sizeMu.Lock()
	stream.width, stream.height = width, height
	stream.sizeMu.Unlock()
}

func (stream *scrcpyVideoStream) videoSize() (int, int, bool) {
	stream.sizeMu.RLock()
	defer stream.sizeMu.RUnlock()
	return stream.width, stream.height, stream.width > 0 && stream.height > 0 && stream.width <= 65535 && stream.height <= 65535
}

// sendControl writes scrcpy's documented binary control protocol directly to
// the server control socket. It avoids launching an adb process per gesture
// and, unlike `adb shell input swipe`, preserves intermediate move events.
func (stream *scrcpyVideoStream) sendControl(event ControlEvent) error {
	message, err := stream.controlMessage(event)
	if err != nil || len(message) == 0 {
		return err
	}
	stream.controlMu.Lock()
	defer stream.controlMu.Unlock()
	if stream.control == nil {
		return errors.New("scrcpy native control channel is unavailable")
	}
	_, err = stream.control.Write(message)
	return err
}

func (stream *scrcpyVideoStream) controlMessage(event ControlEvent) ([]byte, error) {
	const (
		controlInjectKeycode = 0
		controlInjectText    = 1
		controlInjectTouch   = 2
		controlInjectScroll  = 3
		pointerFingerID      = ^uint64(1) // scrcpy's SC_POINTER_ID_GENERIC_FINGER
	)
	if event.Type == "pointer-down" || event.Type == "pointer-move" || event.Type == "pointer-up" {
		width, height, ok := stream.videoSize()
		if !ok {
			return nil, errors.New("scrcpy video dimensions are not ready")
		}
		action := byte(0)
		pressure := uint16(0xffff)
		switch event.Type {
		case "pointer-move":
			action = 2
		case "pointer-up":
			action, pressure = 1, 0
		}
		message := make([]byte, 32)
		message[0], message[1] = controlInjectTouch, action
		binary.BigEndian.PutUint64(message[2:10], pointerFingerID)
		binary.BigEndian.PutUint32(message[10:14], uint32(normalizedCoordinate(event.X, width)))
		binary.BigEndian.PutUint32(message[14:18], uint32(normalizedCoordinate(event.Y, height)))
		binary.BigEndian.PutUint16(message[18:20], uint16(width))
		binary.BigEndian.PutUint16(message[20:22], uint16(height))
		binary.BigEndian.PutUint16(message[22:24], pressure)
		return message, nil
	}
	if event.Type == "scroll" {
		width, height, ok := stream.videoSize()
		if !ok {
			return nil, errors.New("scrcpy video dimensions are not ready")
		}
		message := make([]byte, 21)
		message[0] = controlInjectScroll
		binary.BigEndian.PutUint32(message[1:5], uint32(width/2))
		binary.BigEndian.PutUint32(message[5:9], uint32(height/2))
		binary.BigEndian.PutUint16(message[9:11], uint16(width))
		binary.BigEndian.PutUint16(message[11:13], uint16(height))
		binary.BigEndian.PutUint16(message[13:15], uint16(signedScroll(event.X)))
		binary.BigEndian.PutUint16(message[15:17], uint16(signedScroll(event.Y)))
		return message, nil
	}
	if event.Type == "system" || event.Type == "key" {
		if event.Type == "key" && len([]rune(event.Key)) == 1 {
			text := []byte(event.Key)
			if len(text) > 300 {
				return nil, errors.New("text input is too long")
			}
			message := make([]byte, 5+len(text))
			message[0] = controlInjectText
			binary.BigEndian.PutUint32(message[1:5], uint32(len(text)))
			copy(message[5:], text)
			return message, nil
		}
		keycode, ok := nativeKeycode(event)
		if !ok {
			return nil, nil
		}
		// Android key input is a down/up pair. Sending both in one socket write
		// preserves order without a second WebRTC/ADB round trip.
		message := make([]byte, 28)
		for index, action := range []byte{0, 1} {
			offset := index * 14
			message[offset] = controlInjectKeycode
			message[offset+1] = action
			binary.BigEndian.PutUint32(message[offset+2:offset+6], keycode)
		}
		return message, nil
	}
	return nil, nil
}

func normalizedCoordinate(value float64, extent int) int {
	if value < 0 {
		value = 0
	} else if value > 1 {
		value = 1
	}
	return int(value * float64(extent-1))
}

func signedScroll(value float64) int16 {
	if value > 0 {
		return 32767
	}
	if value < 0 {
		return -32768
	}
	return 0
}

func nativeKeycode(event ControlEvent) (uint32, bool) {
	keys := map[string]uint32{"power": 26, "volume-up": 24, "volume-down": 25, "back": 4, "home": 3, "recents": 187, "Enter": 66, "Backspace": 67, "Escape": 4}
	key, ok := keys[event.Key]
	return key, ok
}

type sampleWriter interface {
	WriteSample(media.Sample) error
}

// latestSampleWriter prevents a constrained WebRTC route from accumulating a
// video backlog. It is intentionally a one-slot mailbox: keeping a frame in a
// queue is worse than dropping it for an interactive remote-control session.
type latestSampleWriter struct {
	output    sampleWriter
	latest    chan media.Sample
	closed    chan struct{}
	finished  chan struct{}
	onDrop    func()
	closeOnce sync.Once
}

func newLatestSampleWriter(output sampleWriter, onDrop func()) *latestSampleWriter {
	writer := &latestSampleWriter{
		output: output, latest: make(chan media.Sample, 1), closed: make(chan struct{}), finished: make(chan struct{}), onDrop: onDrop,
	}
	go func() {
		defer close(writer.finished)
		for {
			select {
			case <-writer.closed:
				return
			case sample := <-writer.latest:
				// The output write is allowed to take time; meanwhile WriteSample
				// keeps replacing the single pending slot with fresher data.
				_ = writer.output.WriteSample(sample)
			}
		}
	}()
	return writer
}

func (writer *latestSampleWriter) WriteSample(sample media.Sample) error {
	select {
	case <-writer.closed:
		return io.ErrClosedPipe
	default:
	}
	select {
	case writer.latest <- sample:
		return nil
	default:
	}
	select {
	case <-writer.latest:
		if writer.onDrop != nil {
			writer.onDrop()
		}
	default:
	}
	select {
	case writer.latest <- sample:
		return nil
	case <-writer.closed:
		return io.ErrClosedPipe
	}
}

func (writer *latestSampleWriter) Close() {
	writer.closeOnce.Do(func() {
		close(writer.closed)
		<-writer.finished
	})
}

func readUint32(reader io.Reader) (uint32, error) {
	var buffer [4]byte
	_, err := io.ReadFull(reader, buffer[:])
	return binary.BigEndian.Uint32(buffer[:]), err
}

func readUint64(reader io.Reader) (uint64, error) {
	var buffer [8]byte
	_, err := io.ReadFull(reader, buffer[:])
	return binary.BigEndian.Uint64(buffer[:]), err
}

func commandMessage(err error, output []byte) string {
	message := strings.TrimSpace(string(output))
	if message != "" {
		return message
	}
	return err.Error()
}
