//go:build race

package redact

func init() { raceEnabled = true }
