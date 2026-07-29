package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// sessionStore is local-only. It lets an unrevoked share survive a normal app
// restart while keeping the secret token out of owner and guest API responses.
type sessionStore struct {
	ActiveID string          `json:"activeId"`
	Sessions []storedSession `json:"sessions"`
}

type storedSession struct {
	Session Session `json:"session"`
	Token   string  `json:"token"`
}

func loadSessionStore(file string, now time.Time) (map[string]*Session, map[string]string, string) {
	sessions := make(map[string]*Session)
	tokens := make(map[string]string)
	data, err := os.ReadFile(file)
	if err != nil {
		return sessions, tokens, ""
	}
	var saved sessionStore
	if json.Unmarshal(data, &saved) != nil {
		return sessions, tokens, ""
	}
	for _, item := range saved.Sessions {
		value := item.Session
		value.Token = item.Token
		value.pointerStart = nil
		value.refresh(now)
		if value.ID == "" || value.Token == "" || value.State == "stopped" || value.State == "expired" {
			continue
		}
		sessions[value.ID] = &value
		tokens[value.Token] = value.ID
	}
	if _, ok := sessions[saved.ActiveID]; ok {
		return sessions, tokens, saved.ActiveID
	}
	return sessions, tokens, ""
}

func saveSessionStore(file, activeID string, sessions map[string]*Session) error {
	if file == "" {
		return nil
	}
	saved := sessionStore{ActiveID: activeID}
	for _, session := range sessions {
		if session == nil || session.State == "stopped" || session.State == "expired" {
			continue
		}
		copy := *session
		copy.pointerStart = nil
		saved.Sessions = append(saved.Sessions, storedSession{Session: copy, Token: session.Token})
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	temporary := file + ".tmp"
	if err = os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, file)
}
