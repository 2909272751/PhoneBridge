package app

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestScrcpyTouchControlMessage(t *testing.T) {
	stream := &scrcpyVideoStream{width: 1080, height: 1920}
	message, err := stream.controlMessage(ControlEvent{Type: "pointer-down", X: 0.5, Y: 0.25})
	if err != nil {
		t.Fatal(err)
	}
	if len(message) != 32 || message[0] != 2 || message[1] != 0 {
		t.Fatalf("unexpected touch header: %v", message)
	}
	if got := binary.BigEndian.Uint64(message[2:10]); got != ^uint64(1) {
		t.Fatalf("pointer id = %#x", got)
	}
	if got := binary.BigEndian.Uint32(message[10:14]); got != 539 {
		t.Fatalf("x = %d", got)
	}
	if got := binary.BigEndian.Uint32(message[14:18]); got != 479 {
		t.Fatalf("y = %d", got)
	}
	if got := binary.BigEndian.Uint16(message[22:24]); got != 0xffff {
		t.Fatalf("pressure = %#x", got)
	}
}

func TestScrcpyKeyControlIsDownUpPair(t *testing.T) {
	message, err := (&scrcpyVideoStream{}).controlMessage(ControlEvent{Type: "system", Key: "home"})
	if err != nil {
		t.Fatal(err)
	}
	if len(message) != 28 || !bytes.Equal(message[:2], []byte{0, 0}) || !bytes.Equal(message[14:16], []byte{0, 1}) {
		t.Fatalf("unexpected key sequence: %v", message)
	}
	if got := binary.BigEndian.Uint32(message[2:6]); got != 3 {
		t.Fatalf("down keycode = %d", got)
	}
	if got := binary.BigEndian.Uint32(message[16:20]); got != 3 {
		t.Fatalf("up keycode = %d", got)
	}
}
