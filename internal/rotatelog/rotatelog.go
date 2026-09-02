// Package rotatelog provides a small, dependency-free size-based log writer.
package rotatelog

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

// Writer appends to path and rotates it before a write would exceed maxBytes.
// Backups are named path.1 through path.N, with .1 always the newest.
type Writer struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	file     *os.File
	size     int64
}

// New opens a rotating writer. maxBytes and backups must both be positive.
func New(path string, maxBytes int64, backups int) (*Writer, error) {
	if path == "" {
		return nil, fmt.Errorf("log path is empty")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("maxBytes must be positive")
	}
	if backups <= 0 {
		return nil, fmt.Errorf("backups must be positive")
	}

	w := &Writer{path: path, maxBytes: maxBytes, backups: backups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	w.file = file
	w.size = info.Size()
	return nil
}

// Write implements io.Writer.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if len(p) > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil

	reopenAfterError := func(rotationErr error) error {
		if err := w.open(); err != nil {
			return fmt.Errorf("rotate: %v; reopen: %w", rotationErr, err)
		}
		return rotationErr
	}

	oldest := fmt.Sprintf("%s.%d", w.path, w.backups)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return reopenAfterError(err)
	}
	for index := w.backups - 1; index >= 1; index-- {
		source := fmt.Sprintf("%s.%d", w.path, index)
		target := fmt.Sprintf("%s.%d", w.path, index+1)
		if err := os.Rename(source, target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return reopenAfterError(err)
		}
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return reopenAfterError(err)
	}
	return w.open()
}

// Close closes the active log file. It is safe to call more than once.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
