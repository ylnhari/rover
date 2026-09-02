package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type failingLogWriter struct{}

func (failingLogWriter) Write([]byte) (int, error) {
	return 0, errors.New("console unavailable")
}

func TestOperationalLogWritesFileBeforeBrokenConsole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rover.log")
	output, closer, err := newOperationalLogOutput(path, failingLogWriter{}, 1024, 3)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fmt.Fprint(output, "diagnostic"); err == nil {
		t.Fatal("write unexpectedly hid the broken console")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "diagnostic" {
		t.Fatalf("file = %q, want diagnostic", data)
	}
}

func TestOperationalLogUsesRequiredRotationPolicy(t *testing.T) {
	if operationalLogMaxBytes != 2*1024*1024 {
		t.Fatalf("max bytes = %d", operationalLogMaxBytes)
	}
	if operationalLogBackups != 3 {
		t.Fatalf("backups = %d", operationalLogBackups)
	}

	path := filepath.Join(t.TempDir(), "rover.log")
	output, closer, err := newOperationalLogOutput(path, os.Stderr, 5, operationalLogBackups)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"one", "two", "three", "four"} {
		if _, err := output.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	for suffix, want := range map[string]string{
		"":   "four",
		".1": "three",
		".2": "two",
		".3": "one",
	} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Fatalf("%s = %q, want %q", path+suffix, data, want)
		}
	}
}
