package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Logger struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	keep       int
	file       *os.File
	size       int64
	alsoStderr bool
}

func New(path string, maxBytes int64, keep int, alsoStderr bool) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &Logger{path: path, maxBytes: maxBytes, keep: keep, alsoStderr: alsoStderr}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Logger) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.file = f
	l.size = info.Size()
	return nil
}

func (l *Logger) Printf(format string, args ...any) {
	line := fmt.Sprintf("%s %s\n", time.Now().Format("2006-01-02T15:04:05.000Z07:00"), fmt.Sprintf(format, args...))

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.maxBytes > 0 && l.size+int64(len(line)) > l.maxBytes {
		l.rotate()
	}
	if l.file != nil {
		n, _ := l.file.WriteString(line)
		l.size += int64(n)
	}
	if l.alsoStderr {
		os.Stderr.WriteString(line)
	}
}

func (l *Logger) rotate() {
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	for i := l.keep; i >= 1; i-- {
		src := l.path
		if i > 1 {
			src = fmt.Sprintf("%s.%d", l.path, i-1)
		}
		dst := fmt.Sprintf("%s.%d", l.path, i)
		os.Remove(dst)
		os.Rename(src, dst)
	}
	_ = l.open()
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}
