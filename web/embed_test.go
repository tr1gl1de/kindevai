package web

import (
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
)

func TestAssetsServesIndex(t *testing.T) {
	t.Parallel()
	assets, _ := Assets()
	f, err := assets.Open("index.html")
	if err != nil {
		t.Fatalf("open index.html: %v", err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !strings.Contains(string(b), "<html") {
		t.Fatalf("index.html does not look like HTML: %q", b)
	}
}

func TestPlaceholderOnlyHasIndex(t *testing.T) {
	t.Parallel()
	_, err := placeholderFS{}.Open("assets/app.js")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}
