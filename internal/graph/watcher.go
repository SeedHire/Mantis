package graph

import (
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/seedhire/mantis/internal/parser"
)

// Watcher watches the project directory for file changes and updates the graph.
type Watcher struct {
	builder *Builder
	root    string
	done    chan struct{}
}

// NewWatcher creates a new Watcher.
func NewWatcher(builder *Builder, root string) *Watcher {
	return &Watcher{
		builder: builder,
		root:    root,
		done:    make(chan struct{}),
	}
}

// Start begins watching all subdirectories recursively.
func (w *Watcher) Start() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	// Add root and all subdirectories
	if err := addDirsRecursive(watcher, w.root); err != nil {
		watcher.Close()
		return err
	}

	go func() {
		defer watcher.Close()

		var debounceMu sync.Mutex
		debounce := map[string]*time.Timer{}

		for {
			select {
			case <-w.done:
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				path := event.Name
				if !isCodeFile(path, w.builder.parsers) {
					continue
				}

				// Debounce 200ms — mutex protects map across goroutines (AfterFunc callbacks).
				debounceMu.Lock()
				if t, exists := debounce[path]; exists {
					t.Stop()
				}
				op := event.Op
				debounce[path] = time.AfterFunc(200*time.Millisecond, func() {
					select {
					case <-w.done:
						return
					default:
					}
					debounceMu.Lock()
					delete(debounce, path)
					debounceMu.Unlock()
					if op&fsnotify.Remove != 0 {
						_ = w.builder.RemoveFile(path)
					} else if op&(fsnotify.Create|fsnotify.Write) != 0 {
						_ = w.builder.UpdateFile(path)
					}
				})
				debounceMu.Unlock()

			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			}
		}
	}()

	return nil
}

// Stop signals the watcher to shut down.
func (w *Watcher) Stop() {
	close(w.done)
}

func addDirsRecursive(watcher *fsnotify.Watcher, root string) error {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if defaultIgnore[filepath.Base(path)] {
				return filepath.SkipDir
			}
			_ = watcher.Add(path)
		}
		return nil
	})
	return nil
}

func isCodeFile(path string, parsers map[string]parser.Parser) bool {
	ext := filepath.Ext(path)
	_, ok := parsers[ext]
	return ok
}
