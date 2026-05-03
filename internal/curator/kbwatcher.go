package curator

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// KnowledgeWatcher emits `project_knowledge_updated` websocket events
// the moment a file under <projectsRoot>/<id>/knowledge-base/ is
// created, modified, removed, or renamed — so the frontend Knowledge
// panel refreshes mid-turn as the curator agent writes notes.
//
// We watch three nested layers because new projects + new
// knowledge-base dirs can appear at runtime:
//
//	<projectsRoot>            → catches new project dirs
//	<projectsRoot>/<id>       → catches the knowledge-base subdir being created
//	<projectsRoot>/<id>/knowledge-base   → catches every file change inside
//
// fsnotify watches are non-recursive on every supported platform, so
// we maintain the inner watches ourselves: on a Create event one level
// up, we add the corresponding watch and (for the kb-dir-creation case)
// fire the event so the frontend renders whatever lands inside.
//
// Per-project debouncing collapses the burst of fs events a single
// agent Write tends to fire (Write often emits Create + Write + Chmod
// in quick succession) into a single broadcast.
const (
	kbDirName         = "knowledge-base"
	knowledgeDebounce = 100 * time.Millisecond
)

// Broadcaster is the subset of *websocket.Hub behavior the watcher needs.
// Stating it as an interface lets tests substitute a recorder without
// wiring a real http server + ws client.
type Broadcaster interface {
	Broadcast(websocket.Event)
}

type KnowledgeWatcher struct {
	watcher *fsnotify.Watcher
	hub     Broadcaster
	root    string

	mu       sync.Mutex
	debounce map[string]*time.Timer
}

// NewKnowledgeWatcher constructs a watcher rooted at projectsRoot and
// starts the event loop. The root is created if missing — fresh
// installs haven't run a curator turn yet, so the dir genuinely may
// not exist.
func NewKnowledgeWatcher(hub Broadcaster, projectsRoot string) (*KnowledgeWatcher, error) {
	if err := os.MkdirAll(projectsRoot, 0o755); err != nil {
		return nil, fmt.Errorf("ensure projects root: %w", err)
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fsnotify: %w", err)
	}
	kw := &KnowledgeWatcher{
		watcher:  w,
		hub:      hub,
		root:     projectsRoot,
		debounce: make(map[string]*time.Timer),
	}
	if err := w.Add(projectsRoot); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("watch root %q: %w", projectsRoot, err)
	}
	if err := kw.seedExistingWatches(); err != nil {
		log.Printf("[kbwatcher] seed existing watches: %v (continuing)", err)
	}
	go kw.run()
	return kw, nil
}

// seedExistingWatches walks the root once at startup, adding watches
// for every existing project dir + every existing knowledge-base
// subdir. Failures on individual subdirs log + continue — a permission
// glitch on one project shouldn't take the watcher down for all.
func (kw *KnowledgeWatcher) seedExistingWatches() error {
	entries, err := os.ReadDir(kw.root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		projectDir := filepath.Join(kw.root, e.Name())
		if err := kw.watcher.Add(projectDir); err != nil {
			log.Printf("[kbwatcher] watch %s: %v", projectDir, err)
			continue
		}
		kbDir := filepath.Join(projectDir, kbDirName)
		if info, err := os.Stat(kbDir); err == nil && info.IsDir() {
			if err := kw.watcher.Add(kbDir); err != nil {
				log.Printf("[kbwatcher] watch %s: %v", kbDir, err)
			}
		}
	}
	return nil
}

func (kw *KnowledgeWatcher) run() {
	for {
		select {
		case event, ok := <-kw.watcher.Events:
			if !ok {
				return
			}
			kw.handle(event)
		case err, ok := <-kw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[kbwatcher] watcher error: %v", err)
		}
	}
}

// handle dispatches a single fsnotify event. Three shapes:
//
//   - <root>/<id>                  → new project dir (Create). Add a watch
//     so we can later catch its knowledge-base subdir.
//   - <root>/<id>/knowledge-base   → kb subdir created (Create). Add a
//     watch and fire — the file that triggered its creation has
//     probably already landed by the time we add the watch, so emit
//     immediately rather than relying on a follow-up event.
//   - <root>/<id>/knowledge-base/* → any file change. Fire (debounced).
//
// fsnotify also delivers Remove events when watched dirs disappear;
// the watcher auto-removes its handle in that case, so we don't have
// to explicitly Unwatch on project deletion.
func (kw *KnowledgeWatcher) handle(event fsnotify.Event) {
	rel, err := filepath.Rel(kw.root, event.Name)
	if err != nil {
		return
	}
	parts := strings.Split(rel, string(filepath.Separator))

	if len(parts) == 1 {
		// <root>/<dir-or-file>. Only care about Create on dirs.
		if event.Op&fsnotify.Create != 0 {
			if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
				if err := kw.watcher.Add(event.Name); err != nil {
					log.Printf("[kbwatcher] watch new project %s: %v", event.Name, err)
				}
				// The new project dir may already contain
				// `knowledge-base/` — `os.MkdirAll(<id>/knowledge-base)`
				// creates both levels in one shot, and project import
				// tooling lays the entire tree down atomically before
				// any of our fs events land. In both cases the inner
				// dir's own Create event was emitted into a watch we
				// hadn't installed yet, so a `len(parts) == 2` branch
				// below would never fire for it. Detect it now and
				// install the inner watch + fire so the panel picks
				// up whatever's already inside.
				kbDir := filepath.Join(event.Name, kbDirName)
				if info2, statErr2 := os.Stat(kbDir); statErr2 == nil && info2.IsDir() {
					if err := kw.watcher.Add(kbDir); err != nil {
						log.Printf("[kbwatcher] watch new kb %s: %v", kbDir, err)
					}
					kw.fire(parts[0])
				}
			}
		}
		return
	}

	if len(parts) >= 2 && parts[1] == kbDirName {
		projectID := parts[0]
		if len(parts) == 2 {
			// <root>/<id>/knowledge-base — the kb dir itself.
			if event.Op&fsnotify.Create != 0 {
				if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
					if err := kw.watcher.Add(event.Name); err != nil {
						log.Printf("[kbwatcher] watch new kb %s: %v", event.Name, err)
					}
				}
				kw.fire(projectID)
			}
			return
		}
		// <root>/<id>/knowledge-base/... — file inside.
		kw.fire(projectID)
	}
}

// fire schedules a debounced broadcast for projectID. Repeated calls
// within `knowledgeDebounce` collapse to a single emission, taking the
// trailing edge of the burst.
func (kw *KnowledgeWatcher) fire(projectID string) {
	kw.mu.Lock()
	defer kw.mu.Unlock()
	if t, ok := kw.debounce[projectID]; ok {
		t.Stop()
	}
	kw.debounce[projectID] = time.AfterFunc(knowledgeDebounce, func() {
		kw.mu.Lock()
		delete(kw.debounce, projectID)
		kw.mu.Unlock()
		if kw.hub == nil {
			return
		}
		kw.hub.Broadcast(websocket.Event{
			Type:      "project_knowledge_updated",
			ProjectID: projectID,
		})
	})
}

// Close stops the watcher. Pending debounce timers are allowed to
// fire — they broadcast onto the hub which is owned upstream and
// will outlive this watcher; broadcasting to a still-valid hub is
// harmless.
func (kw *KnowledgeWatcher) Close() error {
	return kw.watcher.Close()
}
