package reload

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsGoFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"main.go", true},
		{"reload_test.go", true},
		{"file.txt", false},
		{"go.mod", false},
		{"go.sum", false},
		{".go", true},
		{"path/to/file.go", true},
		{"path/to/file.js", false},
	}
	for _, tt := range tests {
		if got := isGoFile(tt.name); got != tt.want {
			t.Errorf("isGoFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSkipDir(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{".git", true},
		{"vendor", true},
		{"bin", true},
		{"node_modules", true},
		{".hidden", true},
		{"internal", false},
		{"cmd", false},
		{"src", false},
	}
	for _, tt := range tests {
		if got := skipDir(tt.name); got != tt.want {
			t.Errorf("skipDir(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDebounce(t *testing.T) {
	// Create a temp directory with a .go file.
	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	os.WriteFile(goFile, []byte("package main\n"), 0644)

	var buildCount atomic.Int32
	w, err := New(Config{
		SourceDir:  dir,
		BinaryPath: "/tmp/fake-binary",
		Debounce:   100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Override build to just count invocations.
	w.buildFunc = func(binaryPath, buildPkg string) error {
		buildCount.Add(1)
		return nil
	}
	// Override restart to be a no-op.
	w.restartFunc = func(binaryPath string) error {
		return nil
	}

	done := make(chan struct{})
	go w.Run(done)

	// Wait for watcher to start.
	time.Sleep(50 * time.Millisecond)

	// Rapid-fire 5 writes — should debounce to 1 build.
	for i := 0; i < 5; i++ {
		os.WriteFile(goFile, []byte("package main // v"+string(rune('0'+i))+"\n"), 0644)
		time.Sleep(10 * time.Millisecond)
	}

	// Wait for debounce to fire.
	time.Sleep(300 * time.Millisecond)
	close(done)

	count := buildCount.Load()
	if count != 1 {
		t.Errorf("expected 1 build after debounce, got %d", count)
	}
}

func TestRestartLoopDetection(t *testing.T) {
	w := &Watcher{
		maxRestarts:   3,
		restartWindow: 1 * time.Second,
	}

	// First 3 restarts should be fine.
	for i := 0; i < 3; i++ {
		if w.detectRestartLoop() {
			t.Fatalf("restart %d should not trigger loop detection", i+1)
		}
	}

	// 4th restart should trigger loop detection.
	if !w.detectRestartLoop() {
		t.Error("4th restart should trigger loop detection")
	}

	// Once disabled, isDisabled should return true.
	if !w.isDisabled() {
		t.Error("watcher should be disabled after restart loop")
	}
}

func TestRestartLoopWindowExpiry(t *testing.T) {
	w := &Watcher{
		maxRestarts:   3,
		restartWindow: 50 * time.Millisecond,
	}

	// 3 restarts within window.
	for i := 0; i < 3; i++ {
		w.detectRestartLoop()
	}

	// Wait for window to expire.
	time.Sleep(100 * time.Millisecond)

	// Next restart should not trigger detection (old entries expired).
	if w.detectRestartLoop() {
		t.Error("restart after window expiry should not trigger loop detection")
	}
}

func TestFindSourceDir(t *testing.T) {
	// Create temp dir with go.mod.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0644)
	subdir := filepath.Join(dir, "internal", "reload")
	os.MkdirAll(subdir, 0755)

	got, err := FindSourceDir(subdir)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("FindSourceDir(%q) = %q, want %q", subdir, got, dir)
	}
}

func TestFindSourceDirNotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := FindSourceDir(dir)
	if err == nil {
		t.Error("expected error when go.mod not found")
	}
}

func TestNonGoFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	// Write a .go file so the watcher has something to watch.
	goFile := filepath.Join(dir, "main.go")
	os.WriteFile(goFile, []byte("package main\n"), 0644)

	var buildCount atomic.Int32
	w, err := New(Config{
		SourceDir:  dir,
		BinaryPath: "/tmp/fake-binary",
		Debounce:   50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.buildFunc = func(binaryPath, buildPkg string) error {
		buildCount.Add(1)
		return nil
	}
	w.restartFunc = func(binaryPath string) error { return nil }

	done := make(chan struct{})
	go w.Run(done)
	time.Sleep(50 * time.Millisecond)

	// Write non-.go files — should not trigger build.
	os.WriteFile(filepath.Join(dir, "readme.md"), []byte("# hi\n"), 0644)
	os.WriteFile(filepath.Join(dir, "data.json"), []byte("{}\n"), 0644)
	time.Sleep(200 * time.Millisecond)
	close(done)

	if buildCount.Load() != 0 {
		t.Errorf("expected 0 builds for non-.go files, got %d", buildCount.Load())
	}
}
