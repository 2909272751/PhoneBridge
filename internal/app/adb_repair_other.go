//go:build !windows

package app

import "context"

func findADBServerProcess(context.Context) (int, string, bool, error) {
	return 0, "", false, errADBRepairUnsupported
}

func forceStopADBProcess(context.Context, int) error {
	return errADBRepairUnsupported
}
