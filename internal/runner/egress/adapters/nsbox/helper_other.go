//go:build !linux

package nsbox

import (
	"fmt"
	"io"
	"syscall"
)

// Strict egress has no verified enforcement outside Linux/WSL2 (macOS: post-MVP); Support reports it unsupported
// and Open refuses.

func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

func namespacesAvailable() bool { return false }

// Main refuses: there is no sandbox on this platform.
func Main(_ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "strict egress sandbox: unsupported on this platform")
	return ExitUsage
}

// RunMain refuses: there is no sandbox on this platform.
func RunMain(stderr io.Writer) int {
	fmt.Fprintln(stderr, "strict egress sandbox: unsupported on this platform")
	return ExitUnverified
}
