//go:build unix && !linux && !darwin

package process

import "errors"

// StartTime is not implemented on this system: adoption falls back to the pid/process-group check.
func StartTime(int) (string, error) {
	return "", errors.New("process start time is not supported on this system")
}
