package reload

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EMSERO/gopherclaw/internal/config"

	"go.uber.org/zap"
)

func TestWatchContextCancelled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	logger := zap.NewNop().Sugar()

	// Create a minimal valid config file
	cfgData := []byte(`{
		"agents": {"defaults": {"model": {"primary": "test/model"}}},
		"gateway": {"port": 18789}
	}`)
	if err := os.WriteFile(cfgPath, cfgData, 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- Watch(logger, ctx, cfgPath, 50*time.Millisecond, func(cfg *config.Root) {})
	}()

	// Give watcher time to start
	time.Sleep(100 * time.Millisecond)

	// Cancel context to stop watcher
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("expected nil error on context cancel, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Watch did not return within timeout")
	}
}

func TestWatchConfigChange(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	logger := zap.NewNop().Sugar()

	// Create initial config
	cfgData := []byte(`{
		"agents": {"defaults": {"model": {"primary": "test/model"}}},
		"gateway": {"port": 18789}
	}`)
	if err := os.WriteFile(cfgPath, cfgData, 0644); err != nil {
		t.Fatal(err)
	}

	var reloadCount atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Watch(logger, ctx, cfgPath, 50*time.Millisecond, func(cfg *config.Root) {
			reloadCount.Add(1)
		})
	}()

	// Give watcher time to start
	time.Sleep(200 * time.Millisecond)

	// Modify the config file
	newCfgData := []byte(`{
		"agents": {"defaults": {"model": {"primary": "test/model-v2"}}},
		"gateway": {"port": 18789}
	}`)
	if err := os.WriteFile(cfgPath, newCfgData, 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for debounce + callback
	time.Sleep(500 * time.Millisecond)

	count := reloadCount.Load()
	if count < 1 {
		t.Errorf("expected at least 1 reload callback, got %d", count)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("expected nil error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Watch did not return within timeout")
	}
}

func TestWatchNonExistentDir(t *testing.T) {
	logger := zap.NewNop().Sugar()
	ctx := context.Background()
	err := Watch(logger, ctx, "/nonexistent/dir/config.json", time.Second, func(cfg *config.Root) {})
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

func TestWatchDifferentFileChange(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	logger := zap.NewNop().Sugar()

	// Create initial config
	cfgData := []byte(`{
		"agents": {"defaults": {"model": {"primary": "test/model"}}},
		"gateway": {"port": 18789}
	}`)
	if err := os.WriteFile(cfgPath, cfgData, 0644); err != nil {
		t.Fatal(err)
	}

	var reloadCount atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Watch(logger, ctx, cfgPath, 50*time.Millisecond, func(cfg *config.Root) {
			reloadCount.Add(1)
		})
	}()

	// Give watcher time to start
	time.Sleep(200 * time.Millisecond)

	// Modify a DIFFERENT file in the same directory (should NOT trigger reload)
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("other"), 0644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)

	count := reloadCount.Load()
	if count != 0 {
		t.Errorf("expected 0 reloads for different file, got %d", count)
	}

	cancel()
	<-errCh
}

func TestWatchInvalidConfigChange(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	logger := zap.NewNop().Sugar()

	// Create initial valid config
	cfgData := []byte(`{
		"agents": {"defaults": {"model": {"primary": "test/model"}}},
		"gateway": {"port": 18789}
	}`)
	if err := os.WriteFile(cfgPath, cfgData, 0644); err != nil {
		t.Fatal(err)
	}

	var reloadCount atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Watch(logger, ctx, cfgPath, 50*time.Millisecond, func(cfg *config.Root) {
			reloadCount.Add(1)
		})
	}()

	time.Sleep(200 * time.Millisecond)

	// Write invalid JSON - should trigger watcher but fail config.Load
	if err := os.WriteFile(cfgPath, []byte("not valid json"), 0644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	// Reload should NOT have been called since config is invalid
	count := reloadCount.Load()
	if count != 0 {
		t.Errorf("expected 0 reloads for invalid config, got %d", count)
	}

	cancel()
	<-errCh
}
