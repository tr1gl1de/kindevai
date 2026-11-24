package process

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// StartTime returns when pid started, as recorded by the kernel (kinfo_proc p_starttime, seconds.microseconds).
// Together with the pid it identifies a process: a reused pid has a later start time (KIN-64).
func StartTime(pid int) (string, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	st := k.Proc.P_starttime
	if st.Sec == 0 && st.Usec == 0 {
		return "", fmt.Errorf("no process %d", pid)
	}
	return fmt.Sprintf("%d.%06d", st.Sec, st.Usec), nil
}
