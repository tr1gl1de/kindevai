package process

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// bootID tells boots apart: the start time counts clock ticks since boot, and the journal outlives a reboot.
var bootID = sync.OnceValue(func() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "unknown-boot"
	}
	return strings.TrimSpace(string(b))
})

// StartTime returns when pid started, as recorded by the kernel: "<boot id>:<field 22 of /proc/<pid>/stat>" (clock
// ticks since that boot). Together with the pid it identifies a process: a reused pid has a later start time, and
// after a reboot every recorded process is a different one (KIN-64).
func StartTime(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	st, err := statStartTime(string(b))
	if err != nil {
		return "", err
	}
	return bootID() + ":" + st, nil
}

// statStartTime parses a /proc/<pid>/stat line. The command name (field 2) may contain spaces and parentheses, so
// the fields are counted from the last ')'.
func statStartTime(stat string) (string, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return "", fmt.Errorf("malformed /proc stat line")
	}
	f := strings.Fields(stat[i+1:]) // f[0] is field 3 (state)
	const field = 22
	if len(f) < field-2 {
		return "", fmt.Errorf("malformed /proc stat line: %d fields", len(f)+2)
	}
	st := f[field-3]
	if _, err := strconv.ParseUint(st, 10, 64); err != nil {
		return "", fmt.Errorf("malformed start time %q", st)
	}
	return st, nil
}
