package logx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unlimitedcode.log")
	logger, err := New(path, 80, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		logger.Printf("entry %02d with enough text to force rotation", i)
	}
	logger.Close()

	rotated := path + ".1"
	if _, err := os.Stat(rotated); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 80 {
		t.Fatalf("active log grew to %d bytes", info.Size())
	}
}
