package app

import "testing"

func TestParseADBDevices(t *testing.T) {
	output := `List of devices attached
R58N1234567 device product:beyond1qlte model:SM_G9730 device:beyond1q transport_id:2
192.168.1.9:5555 unauthorized product:demo model:Pixel_8 transport_id:5
`
	devices := parseADBDevices(output)
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}
	if devices[0].Model != "SM G9730" || devices[0].State != "device" {
		t.Fatalf("unexpected first device: %#v", devices[0])
	}
	if devices[1].State != "unauthorized" {
		t.Fatalf("unexpected second state: %s", devices[1].State)
	}
}
