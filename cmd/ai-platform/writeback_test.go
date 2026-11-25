package main

import "testing"

// KIN-38: the write-back's comments link to the task page on the first public origin.
func TestTaskLinkBase(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":                                       "",
		"https://kd.example.com":                 "https://kd.example.com",
		" https://kd.example.com/x , https://b ": "https://kd.example.com",
		"ftp://kd":                               "",
		"kd.example.com":                         "",
	} {
		if got := taskLinkBase(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
