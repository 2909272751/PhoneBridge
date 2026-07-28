package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type ScrcpySnapshot struct {
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	Message   string `json:"message"`
}

func discoverScrcpy(explicitPath string) ScrcpySnapshot {
	for _, candidate := range scrcpyCandidates(explicitPath) {
		if candidate == "" {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		return inspectScrcpy(candidate)
	}
	return ScrcpySnapshot{
		Message: "未找到内置 scrcpy 组件。请重新安装 PhoneBridge，应用不会使用系统环境中的未知版本。",
	}
}

func scrcpyCandidates(explicitPath string) []string {
	candidates := []string{explicitPath}
	if executable, err := os.Executable(); err == nil {
		base := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(base, "runtime", "scrcpy", "scrcpy.exe"),
			filepath.Join(base, "scrcpy", "scrcpy.exe"),
			filepath.Clean(filepath.Join(base, "..", "third_party", "scrcpy", "scrcpy-win64-v4.1", "scrcpy.exe")),
		)
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(workingDirectory, "third_party", "scrcpy", "scrcpy-win64-v4.1", "scrcpy.exe"))
	}
	return candidates
}

func inspectScrcpy(path string) ScrcpySnapshot {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	version := ""
	if len(lines) > 0 {
		version = strings.TrimSpace(lines[0])
	}
	if err != nil {
		return ScrcpySnapshot{
			Path:    path,
			Version: version,
			Message: "内置 scrcpy 无法启动，请检查组件是否完整",
		}
	}
	return ScrcpySnapshot{
		Available: true,
		Path:      path,
		Version:   version,
		Message:   "内置 scrcpy 已通过启动检查",
	}
}

func bundledADBPath(scrcpyPath string) string {
	if scrcpyPath == "" {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(scrcpyPath), "adb.exe")
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate
	}
	return ""
}

func bundledScrcpyServerPath(scrcpyPath string) string {
	if scrcpyPath == "" {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(scrcpyPath), "scrcpy-server")
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate
	}
	return ""
}
