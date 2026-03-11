package reload

import (
	"context"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/fsnotify/fsnotify"

	"github.com/EMSERO/gopherclaw/internal/config"
)

// OnReload is called when the config file changes. The new config is passed.
type OnReload func(cfg *config.Root)

// Watch watches the config file for changes and calls onReload with debouncing.
// Blocks until ctx is cancelled.
func Watch(logger *zap.SugaredLogger, ctx context.Context, cfgPath string, debounce time.Duration, onReload OnReload) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()

	// Watch the directory (not the file) to handle editors that do atomic writes
	// (write to tmp, rename). Watching the directory catches renames.
	dir := filepath.Dir(cfgPath)
	base := filepath.Base(cfgPath)
	if err := watcher.Add(dir); err != nil {
		return err
	}

	logger.Infof("reload: watching %s for changes (debounce %s)", cfgPath, debounce)

	var timer *time.Timer

	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			// Only react to writes/creates/renames of the config file itself
			if filepath.Base(event.Name) != base {
				continue
			}
			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
				continue
			}

			// Debounce: reset timer on each event
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, func() {
				logger.Infof("reload: config file changed, reloading...")
				newCfg, err := config.Load(cfgPath)
				if err != nil {
					logger.Errorf("reload: failed to parse config: %v", err)
					return
				}
				onReload(newCfg)
				logger.Infof("reload: config reloaded successfully")
			})

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			logger.Errorf("reload: watcher error: %v", err)
		}
	}
}
