package rotatelog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriterRotatesAndRetainsNewestBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rover.log")
	w, err := New(path, 5, 2)
	if err != nil {
		t.Fatal(err)
	}

	for _, value := range []string{"one", "two", "three"} {
		if _, err := w.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	assertFileContents(t, path, "three")
	assertFileContents(t, path+".1", "two")
	assertFileContents(t, path+".2", "one")
}

func TestWriterRejectsInvalidConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rover.log")
	for _, tc := range []struct {
		path     string
		maxBytes int64
		backups  int
	}{
		{"", 1, 1},
		{path, 0, 1},
		{path, 1, 0},
	} {
		if _, err := New(tc.path, tc.maxBytes, tc.backups); err == nil {
			t.Errorf("New(%q, %d, %d) succeeded", tc.path, tc.maxBytes, tc.backups)
		}
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
