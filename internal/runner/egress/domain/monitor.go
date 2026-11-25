package domain

// Runtime monitoring (KIN-81, KB KIN-A-22 «Во время работы»): while a strict attempt runs, the runner checks its
// egress again now and then (the same proxy and egress IP check as the preflight, from the runner through the
// upstream proxy). When the egress is no longer the verified one the proxy adapter refuses new connections and
// cuts the open ones, and the attempt ends with code network (the step waits for the network).

// CodeIPMismatch is the check code of an egress IP other than the expected one (the Test Network code).
const CodeIPMismatch = "egress_ip_mismatch"

// MonitorFailures is how many runtime checks in a row may fail for another reason (the proxy or the diagnostic
// endpoint unreachable) before the sandbox counts as lost: one brief outage does not end a long attempt.
const MonitorFailures = 2

// Watch decides from the runtime checks of one sandbox whether it lost its verified egress.
type Watch struct{ failures int }

// Observe takes a check's code ("" when it passed) and reports whether the sandbox must be cut off: at once for a
// wrong egress IP, after MonitorFailures failures in a row otherwise.
func (w *Watch) Observe(code string) bool {
	switch code {
	case "":
		w.failures = 0
		return false
	case CodeIPMismatch:
		return true
	}
	w.failures++
	return w.failures >= MonitorFailures
}
