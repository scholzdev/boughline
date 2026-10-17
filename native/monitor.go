package boughline

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

type monitorReply struct {
	State  string            `json:"state"`
	Status map[string]string `json:"status"`
	Error  string            `json:"error,omitempty"`
}
type monitorQuery struct {
	ID    string
	Reply chan monitorReply
}

func (w *Workspace) monitorSocket() string {
	return filepath.Join(os.TempDir(), "bough-watch-"+Hash([]byte(w.Root))[:24]+".sock")
}
func (w *Workspace) monitoredStatus() (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", w.monitorSocket())
	}}
	defer transport.CloseIdleConnections()
	request, _ := http.NewRequestWithContext(ctx, "GET", "http://bough-watch/status", nil)
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var reply monitorReply
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxRequest))
	if err != nil {
		return nil, err
	}
	if err = jsonDecode(raw, &reply); err != nil {
		return nil, err
	}
	if reply.Error != "" || reply.State != Hash(Encode(w.State)) {
		return nil, errors.New("watcher invalidated")
	}
	return reply.Status, nil
}

// Watch is an unprivileged, per-workspace monitor. Each query places a barrier in
// the same OS event queue before consulting the dirty set. State changes, unknown
// paths, watcher failures and overflow invalidate it and trigger a full scan.
// An absent, stale or slow monitor always falls back to an ordinary verified scan.
func (w *Workspace) Watch(ctx context.Context) error {
	watcher, err := fsnotify.NewBufferedWatcher(4096)
	if err != nil {
		return err
	}
	defer watcher.Close()
	reliable := true
	install := func() error {
		trackedDirs := map[string]bool{"": true}
		for p := range w.State.Files {
			for parent := filepath.Dir(p); parent != "."; parent = filepath.Dir(parent) {
				trackedDirs[filepath.ToSlash(parent)] = true
			}
		}
		ignored := w.ignoreMatcher()
		return filepath.WalkDir(w.Root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(w.Root, name)
			rel = filepath.ToSlash(rel)
			if rel == ".git" {
				return filepath.SkipDir
			}
			if rel == ".bough" {
				if err = watcher.Add(name); err != nil {
					return err
				}
				return filepath.SkipDir
			}
			if rel != "." && ignored(rel) && !trackedDirs[rel] {
				return filepath.SkipDir
			}
			relevant := rel == "." || Selected(rel, w.State.Prefixes)
			for _, p := range w.State.Prefixes {
				if under(p, rel) {
					relevant = true
				}
			}
			if !relevant {
				return filepath.SkipDir
			}
			return watcher.Add(name)
		})
	}
	if err = install(); err != nil {
		return err
	}
	socket := w.monitorSocket()
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fail(409, "monitor socket path is occupied")
		}
		if connection, err := net.DialTimeout("unix", socket, 100*time.Millisecond); err == nil {
			connection.Close()
			return fail(409, "workspace watcher is already running")
		}
		if err = os.Remove(socket); err != nil {
			return err
		}
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	queries := make(chan monitorQuery, 16)
	handler := http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" || r.Method != "GET" {
			http.NotFound(out, r)
			return
		}
		query := monitorQuery{ID: RequestID(), Reply: make(chan monitorReply, 1)}
		select {
		case queries <- query:
		case <-r.Context().Done():
			return
		}
		select {
		case reply := <-query.Reply:
			writeJSON(out, 200, reply)
		case <-r.Context().Done():
		}
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	defer server.Close()
	go server.Serve(listener)
	baseline := ""
	dirty := map[string]bool{}
	result := map[string]string{}
	invalid := true
	pending := map[string]chan monitorReply{}
	status := func() monitorReply {
		raw, err := os.ReadFile(filepath.Join(w.Root, ".bough/state.json"))
		if err != nil {
			return monitorReply{Error: err.Error()}
		}
		var state WorkspaceState
		if err = jsonDecode(raw, &state); err != nil {
			return monitorReply{Error: err.Error()}
		}
		identity := Hash(Encode(state))
		if identity != baseline {
			w.State = state
			invalid = true
		}
		if invalid || !reliable {
			if err = install(); err != nil {
				reliable = false
			}
			result, err = w.fullStatus()
			if err != nil {
				return monitorReply{Error: err.Error()}
			}
			dirty = map[string]bool{}
			for path := range result {
				if _, tracked := state.Files[path]; tracked {
					dirty[path] = true
				}
			}
			baseline = identity
			invalid = false
		} else {
			for path := range dirty {
				old, tracked := state.Files[path]
				if !tracked {
					invalid = true
					continue
				}
				current, _, err := ReadLocal(w.Root, path)
				if err != nil {
					return monitorReply{Error: err.Error()}
				}
				if current == nil {
					result[path] = "D"
				} else if *current != old {
					result[path] = "M"
				} else {
					delete(result, path)
					delete(dirty, path)
				}
			}
		}
		copy := map[string]string{}
		for p, s := range result {
			copy[p] = s
		}
		return monitorReply{State: baseline, Status: copy}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case query := <-queries:
			if len(pending) >= 64 {
				query.Reply <- monitorReply{Error: "watcher is busy"}
				continue
			}
			pending[query.ID] = query.Reply
			marker := filepath.Join(w.Root, ".bough", "barrier-"+query.ID)
			if err = os.WriteFile(marker, []byte(query.ID), 0600); err != nil {
				delete(pending, query.ID)
				query.Reply <- monitorReply{Error: err.Error()}
			}
		case event, ok := <-watcher.Events:
			if !ok {
				return fail(503, "workspace watcher closed")
			}
			relative, _ := filepath.Rel(w.Root, event.Name)
			p := filepath.ToSlash(relative)
			if strings.HasPrefix(p, ".bough/barrier-") {
				id := strings.TrimPrefix(p, ".bough/barrier-")
				if reply := pending[id]; reply != nil {
					delete(pending, id)
					reply <- status()
					_ = os.Remove(event.Name)
				}
				continue
			}
			if p == ".bough/state.json" {
				invalid = true
				continue
			}
			if p == ".bough" || strings.HasPrefix(p, ".bough/") {
				continue
			}
			if _, tracked := w.State.Files[p]; tracked {
				dirty[p] = true
			} else {
				invalid = true
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return fail(503, "workspace watcher closed")
			}
			invalid = true
			reliable = false
		}
	}
}
