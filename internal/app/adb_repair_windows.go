//go:build windows

package app

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func findADBServerProcess(parent context.Context) (int, string, bool, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "netstat.exe", "-ano", "-p", "tcp")
	hideBackgroundProcess(command)
	output, err := command.Output()
	if err != nil {
		return 0, "", false, err
	}
	pid, found := parseADBListenerPID(string(output))
	if !found {
		return 0, "", false, nil
	}
	name, err := windowsProcessName(ctx, pid)
	if err != nil {
		return 0, "", false, err
	}
	return pid, name, true, nil
}

func parseADBListenerPID(output string) (int, bool) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.EqualFold(fields[0], "TCP") {
			continue
		}
		localAddress := fields[1]
		state := fields[len(fields)-2]
		if !strings.HasSuffix(localAddress, ":5037") || !strings.EqualFold(state, "LISTENING") {
			continue
		}
		pid, err := strconv.Atoi(fields[len(fields)-1])
		if err == nil && pid > 0 {
			return pid, true
		}
	}
	return 0, false
}

func windowsProcessName(parent context.Context, pid int) (string, error) {
	command := exec.CommandContext(parent, "tasklist.exe", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
	hideBackgroundProcess(command)
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	reader := csv.NewReader(strings.NewReader(strings.TrimSpace(string(output))))
	record, err := reader.Read()
	if err != nil || len(record) < 2 {
		return "", errors.New("无法读取端口占用进程")
	}
	name := strings.TrimSpace(record[0])
	if name == "" {
		return "", errors.New("端口占用进程名称为空")
	}
	return name, nil
}

func forceStopADBProcess(parent context.Context, pid int) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "taskkill.exe", "/PID", strconv.Itoa(pid), "/T", "/F")
	hideBackgroundProcess(command)
	output, err := command.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return fmt.Errorf("%s", detail)
		}
	}
	return err
}
