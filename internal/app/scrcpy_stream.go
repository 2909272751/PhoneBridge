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
	videoProfileAuto    videoProfile = "auto"
	videoProfileSmooth  videoProfile = "smooth"
	videoProfileQuality videoProfile = "quality"
)

type videoProfileSettings struct {
	bitrate int
	maxSize int
	maxFPS  int
}

func normalizeVideoProfile(value string) videoProfile {
	if strings.EqualFold(strings.TrimSpace(value), string(videoProfileSmooth)) {
		return videoProfileSmooth
	}
	if strings.EqualFold(strings.TrimSpace(value), string(videoProfileQuality)) {
		return videoProfileQuality
	}
	return videoProfileAuto
}

func settingsForVideoProfile(profile videoProfile) videoProfileSettings {
	if profile == videoProfileQuality {
		return videoProfileSettings{bitrate: 2_500_000, maxSize: 960, maxFPS: 24}
	}
	if profile == videoProfileSmooth {
		return videoProfileSettings{bitrate: 600_000, maxSize: 360, maxFPS: 10}
	}
	// A public remote-control session values freshness over pixel-perfect
	// quality. These settings leave headroom for an 88FRP/WebRTC UDP route.
	return videoProfileSettings{bitrate: 900_000, maxSize: 540, maxFPS: 18}
}

// scrcpyVideoStream reads the bundled scrcpy server protocol directly. The
// device encodes H.264 with MediaCodec; no desktop window, screenshot loop or
// H.264 decoder is inserted on the host path.
type scrcpyVideoStream struct {
	adbPath  string
	deviceID string
	server   string
	profile  videoProfileSettings
	port     int
	command  *exec.Cmd
	output   bytes.Buffer
	done     chan struct{}
	waitMu   sync.RWMutex
	waitErr  error
	conn     net.Conn
	cancel   context.CancelFunc
}

func startScrcpyVideoStream(parent context.Context, adbPath, deviceID, serverPath string, requestedProfile string) (*scrcpyVideoStream, error) {
	if strings.TrimSpace(serverPath) == "" {
		return nil, errors.New("未找到内置 scrcpy 组件")
	}
	ctx, cancel := context.WithCancel(parent)
	profile := settingsForVideoProfile(normalizeVideoProfile(requestedProfile))
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
		"audio=false", "control=false", "tunnel_forward=true",
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
