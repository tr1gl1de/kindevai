package process

import "testing"

func TestStatStartTime(t *testing.T) {
	t.Parallel()
	line := "4242 (a b) c)) S 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10 18446744073709551615"
	if got, err := statStartTime(line); err != nil || got != "987654" {
		t.Fatalf("start = %q, %v", got, err)
	}
	for _, bad := range []string{"", "1 (x) S 1 2", "1 (x) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 x 1"} {
		if got, err := statStartTime(bad); err == nil {
			t.Errorf("%q parsed as %q", bad, got)
		}
	}
}
