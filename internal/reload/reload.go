package reload

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher watches Go source files and triggers rebuild + restart on changes.
type Watcher struct {
	sourceDir  string
	binaryPath string
	buildPkg   string
	debounce   time.Duration
	onShutdown func()

	// restart-loop detection
	mu             sync.Mutex
	restartTimes   []time.Time
	maxRestarts    int
	restartWindow  time.Duration
	disabled       bool

	// for testing: injectable hooks
	buildFunc   func(binaryPath, buildPkg string) error
	restartFunc func(binaryPath string) error
}

// Config configures the file watcher.
type Config struct {
	SourceDir  string
	BinaryPath string        // path to current binary (default: os.Executable())
	BuildPkg   string        // build target (default: "./cmd/relay")
	Debounce   time.Duration // debounce interval (default: 500ms)
	OnShutdown func()        // called before exec to let daemon clean up
}

// New creates a new Watcher.
func New(cfg Config) (*Watcher, error) {
	if cfg.SourceDir == "" {
		return nil, fmt.Errorf("reload: source_dir is required")
	}
	if cfg.BinaryPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("reload: resolving executable: %w", err)
		}
		cfg.BinaryPath = exe
	}
	if cfg.BuildPkg == "" {
		cfg.BuildPkg = "./cmd/relay"
	}
	if cfg.Debounce == 0 {
		cfg.Debounce = 500 * time.Millisecond
	}
	if cfg.OnShutdown == nil {
		cfg.OnShutdown = func() {}
	}

	return &Watcher{
		sourceDir:     cfg.SourceDir,
		binaryPath:    cfg.BinaryPath,
		buildPkg:      cfg.BuildPkg,
		debounce:      cfg.Debounce,
		onShutdown:    cfg.OnShutdown,
		maxRestarts:   5,
		restartWindow: 60 * time.Second,
		buildFunc:     defaultBuild,
		restartFunc:   defaultRestart,
	}, nil
}

// Run starts watching and blocks until ctx is done or a restart occurs.
func (w *Watcher) Run(done <-chan struct{}) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("reload: creating watcher: %w", err)
	}
	defer watcher.Close()

	// Walk source directory and add all directories (fsnotify is not recursive).
	if err := w.addDirectories(watcher); err != nil {
		return fmt.Errorf("reload: adding directories: %w", err)
	}

	slog.Info("reload: watching for source changes", "dir", w.sourceDir, "debounce", w.debounce)

	var timer *time.Timer
	for {
		select {
		case <-done:
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if !isGoFile(event.Name) {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}

			slog.Debug("reload: source change detected", "file", event.Name, "op", event.Op)

			// Debounce: reset timer on each event.
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(w.debounce, func() {
				w.rebuildAndRestart()
			})

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.Warn("reload: watcher error", "error", err)
		}
	}
}

func (w *Watcher) rebuildAndRestart() {
	if w.isDisabled() {
		slog.Warn("reload: auto-reload disabled due to restart loop")
		return
	}

	slog.Info("reload: source change detected, rebuilding...")

	stagingPath := w.binaryPath + ".reload.tmp"
	if err := w.buildFunc(stagingPath, w.buildPkg); err != nil {
		slog.Error("reload: build failed, keeping current binary", "error", err)
		return
	}

	slog.Info("reload: build succeeded, restarting...")

	if err := os.Rename(stagingPath, w.binaryPath); err != nil {
		slog.Error("reload: failed to replace binary", "error", err)
		os.Remove(stagingPath)
		return
	}

	if w.detectRestartLoop() {
		slog.Error("reload: restart loop detected (>5 restarts in 60s), disabling auto-reload")
		return
	}

	w.onShutdown()

	if err := w.restartFunc(w.binaryPath); err != nil {
		slog.Error("reload: failed to exec new binary", "error", err)
	}
}

func (w *Watcher) addDirectories(watcher *fsnotify.Watcher) error {
	return filepath.WalkDir(w.sourceDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if skipDir(filepath.Base(path)) {
			return filepath.SkipDir
		}
		return watcher.Add(path)
	})
}

// detectRestartLoop records a restart and returns true if the loop threshold is exceeded.
func (w *Watcher) detectRestartLoop() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	w.restartTimes = append(w.restartTimes, now)

	// Trim old entries outside the window.
	cutoff := now.Add(-w.restartWindow)
	filtered := w.restartTimes[:0]
	for _, t := range w.restartTimes {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	w.restartTimes = filtered

	if len(w.restartTimes) > w.maxRestarts {
		w.disabled = true
		return true
	}
	return false
}

func (w *Watcher) isDisabled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.disabled
}

func defaultBuild(binaryPath, buildPkg string) error {
	cmd := exec.Command("go", "build", "-o", binaryPath, buildPkg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func defaultRestart(binaryPath string) error {
	args := os.Args
	env := os.Environ()
	return syscall.Exec(binaryPath, args, env)
}

func isGoFile(name string) bool {
	return strings.HasSuffix(name, ".go")
}

func skipDir(name string) bool {
	switch name {
	case ".git", "vendor", "bin", "node_modules":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// FindSourceDir walks up from the given directory to find a go.mod file,
// returning the directory containing it.
func FindSourceDir(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("reload: go.mod not found walking up from %s", startDir)
		}
		dir = parent
	}
}
