package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type ADBRepairResult struct {
	Success  bool        `json:"success"`
	Message  string      `json:"message"`
	Steps    []string    `json:"steps"`
	Snapshot ADBSnapshot `json:"adb"`
}

// repairADB takes ownership of the local ADB server without touching unrelated
// applications. A normal kill-server is attempted first. Forced termination is
// only allowed when the process listening on 5037 is recognisably an ADB binary.
func repairADB(parent context.Context, adbPath string, demo bool) ADBRepairResult {
	result := ADBRepairResult{Steps: []string{}}
	stopContext, stopCancel := context.WithTimeout(parent, 3*time.Second)
	stopCommand := exec.CommandContext(stopContext, adbPath, "kill-server")
	hideBackgroundProcess(stopCommand)
	stopOutput, stopErr := stopCommand.CombinedOutput()
	stopCancel()
	if stopErr == nil {
		result.Steps = append(result.Steps, "已正常停止旧 ADB 服务")
	} else {
		pid, processName, found, inspectErr := findADBServerProcess(parent)
		if inspectErr != nil {
			return failedADBRepair(result, fmt.Sprintf("无法检查 ADB 端口占用：%v", inspectErr), adbPath, demo)
		}
		if found {
			if !isADBProcessName(processName) {
				return failedADBRepair(result, fmt.Sprintf("端口 5037 被 %s（PID %d）占用，为避免误关程序已停止修复", processName, pid), adbPath, demo)
			}
			if err := forceStopADBProcess(parent, pid); err != nil {
				return failedADBRepair(result, fmt.Sprintf("无法结束冲突的 %s（PID %d）：%v", processName, pid, err), adbPath, demo)
			}
			result.Steps = append(result.Steps, fmt.Sprintf("已结束冲突的 %s（PID %d）", processName, pid))
		} else {
			detail := strings.TrimSpace(string(stopOutput))
			if detail != "" {
				result.Steps = append(result.Steps, "旧 ADB 服务未正常响应："+detail)
			} else {
				result.Steps = append(result.Steps, "未发现仍在监听 5037 的 ADB 服务")
			}
		}
	}

	startContext, startCancel := context.WithTimeout(parent, 15*time.Second)
	startCommand := exec.CommandContext(startContext, adbPath, "start-server")
	hideBackgroundProcess(startCommand)
	startOutput, startErr := startCommand.CombinedOutput()
	startCancel()
	if startErr != nil {
		detail := strings.TrimSpace(string(startOutput))
		if detail == "" {
			detail = startErr.Error()
		}
		return failedADBRepair(result, "内置 ADB 启动失败："+detail, adbPath, demo)
	}
	result.Steps = append(result.Steps, "已启动 PhoneBridge 内置 ADB 服务")

	// Give Windows USB enumeration a brief moment after the server takes over.
	select {
	case <-parent.Done():
	case <-time.After(500 * time.Millisecond):
	}
	result.Snapshot = discoverADB(parent, adbPath, demo)
	result.Success = result.Snapshot.Available
	switch {
	case !result.Snapshot.Available:
		result.Message = "ADB 服务仍未恢复，请检查 Windows USB 驱动"
	case len(result.Snapshot.Devices) == 0:
		result.Message = "ADB 已恢复；请解锁手机并确认“允许 USB 调试”"
	default:
		result.Message = fmt.Sprintf("ADB 已恢复，检测到 %d 台设备", len(result.Snapshot.Devices))
	}
	return result
}

func failedADBRepair(result ADBRepairResult, message, adbPath string, demo bool) ADBRepairResult {
	result.Success = false
	result.Message = message
	result.Snapshot = discoverADB(context.Background(), adbPath, demo)
	return result
}

func isADBProcessName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.HasSuffix(name, ".exe") && strings.Contains(name, "adb")
}

var errADBRepairUnsupported = errors.New("当前系统不支持强制接管 ADB 服务")
