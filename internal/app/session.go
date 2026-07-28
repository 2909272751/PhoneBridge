package app

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math/big"
	"time"
)

type Session struct {
	ID             string     `json:"id"`
	Token          string     `json:"-"`
	AccessCode     string     `json:"accessCode,omitempty"`
	DeviceID       string     `json:"deviceId"`
	DeviceName     string     `json:"deviceName"`
	State          string     `json:"state"`
	Mode           string     `json:"mode"`
	RequireCode    bool       `json:"requireCode"`
	AllowClipboard bool       `json:"allowClipboard"`
	AllowAudio     bool       `json:"allowAudio"`
	StreamProfile  string     `json:"streamProfile"`
	IsDemo         bool       `json:"isDemo"`
	ViewerState    string     `json:"viewerState"`
	ConnectionMode string     `json:"connectionMode"`
	CreatedAt      time.Time  `json:"createdAt"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	StoppedAt      *time.Time `json:"stoppedAt,omitempty"`
	ShareURL       string     `json:"shareUrl"`
	LastEventAt    *time.Time `json:"lastEventAt,omitempty"`
	pointerStart   *PointerPoint
}

type PointerPoint struct {
	X float64
	Y float64
}

type CreateSessionRequest struct {
	DeviceID       string `json:"deviceId"`
	DurationMinute int    `json:"durationMinutes"`
	Mode           string `json:"mode"`
	RequireCode    bool   `json:"requireCode"`
	AllowClipboard bool   `json:"allowClipboard"`
	AllowAudio     bool   `json:"allowAudio"`
	StreamProfile  string `json:"streamProfile"`
}

func newSession(request CreateSessionRequest, device Device, baseURL string, now time.Time) (Session, error) {
	id, err := randomToken(12)
	if err != nil {
		return Session{}, err
	}
	token, err := randomToken(24)
	if err != nil {
		return Session{}, err
	}
	code := ""
	if request.RequireCode {
		code, err = randomDigits(6)
		if err != nil {
			return Session{}, err
		}
	}
	mode := request.Mode
	if mode != "view" {
		mode = "control"
	}
	profile := normalizeVideoProfile(request.StreamProfile)

	var expiresAt *time.Time
	if request.DurationMinute > 0 {
		value := now.Add(time.Duration(request.DurationMinute) * time.Minute)
		expiresAt = &value
	}
	return Session{
		ID:             id,
		Token:          token,
		AccessCode:     code,
		DeviceID:       device.ID,
		DeviceName:     device.Model,
		State:          "waiting",
		Mode:           mode,
		RequireCode:    request.RequireCode,
		AllowClipboard: request.AllowClipboard,
		AllowAudio:     request.AllowAudio,
		StreamProfile:  string(profile),
		IsDemo:         device.IsDemo,
		ViewerState:    "waiting",
		ConnectionMode: "not-connected",
		CreatedAt:      now,
		ExpiresAt:      expiresAt,
		ShareURL:       fmt.Sprintf("%s/s/%s", baseURL, token),
	}, nil
}

func randomToken(byteLength int) (string, error) {
	buffer := make([]byte, byteLength)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func randomDigits(length int) (string, error) {
	result := make([]byte, length)
	for index := range result {
		number, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		result[index] = byte('0' + number.Int64())
	}
	return string(result), nil
}

func (session *Session) refresh(now time.Time) {
	if session.State == "stopped" || session.State == "expired" {
		return
	}
	if session.ExpiresAt != nil && !now.Before(*session.ExpiresAt) {
		session.State = "expired"
		session.ViewerState = "disconnected"
		session.ConnectionMode = "not-connected"
	}
}
