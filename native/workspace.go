package boughline

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

type PendingCommit struct {
	Request CommitRequest   `json:"request"`
	Files   map[string]File `json:"files"`
}
type WorkspaceState struct {
	Version  int             `json:"version"`
	Server   string          `json:"server"`
	Repo     string          `json:"repo"`
	Socket   string          `json:"socket"`
	Revision string          `json:"revision"`
	Prefixes []string        `json:"prefixes"`
	Files    map[string]File `json:"files"`
	Staged   []string        `json:"staged"`
	Pending  *PendingCommit  `json:"pending,omitempty"`
}
type Workspace struct {
	Root   string
	State  WorkspaceState
	Client *Client
}

func SafePath(root, p string) (string, error) {
	if err := ValidPath(p, false); err != nil {
		return "", err
	}
	parts := strings.Split(p, "/")
	current := root
	for i, name := range parts {
		current = filepath.Join(current, name)
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOTDIR) {
			return "", err
		}
		if err == nil && i < len(parts)-1 && info.Mode()&os.ModeSymlink != 0 {
			return "", fail(409, "symlink ancestor is not a checkout directory", p)
		}
	}
	return current, nil
}
func ReadLocal(root, p string) (*File, []byte, error) {
	if _, err := SafePath(root, p); err != nil {
		return nil, nil, err
	}
	directory, err := os.OpenRoot(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer directory.Close()
	info, err := directory.Lstat(p)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	mode := Regular
	var data []byte
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := directory.Readlink(p)
		if err != nil {
			return nil, nil, err
		}
		data = []byte(target)
		mode = Symlink
	} else {
		file, err := directory.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, nil, err
		}
		defer file.Close()
		info, err = file.Stat()
		if err != nil {
			return nil, nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, nil, fail(409, "tracked path is not a regular file", p)
		}
		data, err = io.ReadAll(io.LimitReader(file, MaxBlob+1))
		if err != nil {
			return nil, nil, err
		}
		if len(data) > MaxBlob {
			return nil, nil, fail(413, "file exceeds 256 MiB", p)
		}
		if info.Mode()&0111 != 0 {
			mode = Executable
		}
	}
	return &File{ID: Hash(data), Mode: mode, Size: int64(len(data))}, data, nil
}
func WriteLocal(root, p string, file File, data []byte) error {
	if err := file.Validate(); err != nil {
		return err
	}
	if file.ID != Hash(data) || file.Size != int64(len(data)) {
		return fail(500, "checkout content identity mismatch", p)
	}
	if _, err := SafePath(root, p); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer directory.Close()
	if info, err := directory.Lstat(p); err == nil {
		if info.IsDir() {
			if err = removeEmptyDirectory(directory, p); err != nil {
				return err
			}
		} else {
			return fail(409, "existing content blocks checkout", p)
		}
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOTDIR) {
		return err
	}
	if err = directory.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	syncParent := func() error {
		parent, err := directory.Open(filepath.Dir(p))
		if err != nil {
			return err
		}
		defer parent.Close()
		return parent.Sync()
	}
	if file.Mode == Symlink {
		if len(data) == 0 || len(data) > 4096 || strings.ContainsRune(string(data), 0) {
			return fail(400, "invalid symlink target")
		}
		if err = directory.Symlink(string(data), p); err != nil {
			return err
		}
		return syncParent()
	}
	temporary := filepath.Join(filepath.Dir(p), ".bough-install-"+RequestID())
	handle, err := directory.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer directory.Remove(temporary)
	if err = handle.Chmod(os.FileMode(file.Mode & 0777)); err == nil {
		_, err = handle.Write(data)
	}
	if err == nil {
		err = handle.Sync()
	}
	closed := handle.Close()
	if err == nil {
		err = closed
	}
	if err != nil {
		return err
	}
	if err = directory.Link(temporary, p); err != nil {
		return err
	}
	return syncParent()
}

// AT_REMOVEDIR keeps a racing regular file from being unlinked when an empty
// checkout directory is replaced. Root bounds ancestor traversal to this workspace.
func removeEmptyDirectory(root *os.Root, p string) error {
	parent, err := root.Open(filepath.Dir(p))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err = unix.Unlinkat(int(parent.Fd()), filepath.Base(p), unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return parent.Sync()
}

func OpenWorkspace(root, token, ca string) (*Workspace, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return nil, fail(400, "workspace root must be a directory, not a symlink")
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	meta := filepath.Join(absolute, ".bough")
	info, err = os.Lstat(meta)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fail(400, "not a Boughline workspace root")
	}
	data, err := os.ReadFile(filepath.Join(meta, "state.json"))
	if err != nil {
		return nil, err
	}
	var state WorkspaceState
	if err = jsonDecode(data, &state); err != nil {
		return nil, err
	}
	if state.Version != 1 {
		return nil, fail(400, "unsupported workspace version")
	}
	for p := range state.Files {
		if err = ValidPath(p, false); err != nil {
			return nil, err
		}
	}
	client, err := NewClient(state.Server, state.Repo, token, state.Socket, filepath.Join(meta, "cache"), ca)
	if err != nil {
		return nil, err
	}
	return &Workspace{Root: absolute, State: state, Client: client}, nil
}
func (w *Workspace) Save() error {
	return AtomicFile(filepath.Join(w.Root, ".bough", "state.json"), Encode(w.State), 0600)
}
func (w *Workspace) Lock(recovering bool) (func(), error) {
	fd, err := unix.Open(filepath.Join(w.Root, ".bough", "lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(fd, unix.LOCK_EX); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	unlock := func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }
	if _, err = os.Stat(filepath.Join(w.Root, ".bough", "recovery.json")); err == nil && !recovering {
		unlock()
		return nil, fail(409, "interrupted update; run bough recover")
	}
	data, err := os.ReadFile(filepath.Join(w.Root, ".bough", "state.json"))
	if err != nil {
		unlock()
		return nil, err
	}
	if err = jsonDecode(data, &w.State); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}
func Clone(ctx context.Context, c *Client, destination, revision string, prefixes []string) (*Workspace, error) {
	root, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(root); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fail(400, "destination must be a new or empty directory")
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			return nil, fail(409, "destination is not empty")
		}
	}
	if err = os.MkdirAll(filepath.Join(root, ".bough"), 0700); err != nil {
		return nil, err
	}
	if len(prefixes) == 0 {
		prefixes = []string{""}
	}
	for _, p := range prefixes {
		if err = ValidPath(p, true); err != nil {
			return nil, err
		}
	}
	checkout, err := c.Checkout(ctx, revision)
	if err != nil {
		return nil, err
	}
	files, err := c.Manifest(ctx, checkout, prefixes)
	if err != nil {
		return nil, err
	}
	for p, f := range files {
		data, err := c.Object(ctx, checkout, "blob", f.ID, p)
		if err != nil {
			return nil, err
		}
		if err = WriteLocal(root, p, f, data); err != nil {
			return nil, err
		}
	}
	w := &Workspace{Root: root, Client: c, State: WorkspaceState{Version: 1, Server: c.Server, Repo: c.Repo, Socket: c.Socket, Revision: checkout.Revision, Prefixes: prefixes, Files: files, Staged: []string{}}}
	if err = w.Save(); err != nil {
		return nil, err
	}
	return w, nil
}
func (w *Workspace) ignoreMatcher() func(string) bool {
	patterns := []string{}
	if raw, err := os.ReadFile(filepath.Join(w.Root, ".boughignore")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				patterns = append(patterns, strings.Trim(line, "/"))
			}
		}
	}
	return func(p string) bool {
		for _, pattern := range patterns {
			if match, _ := filepath.Match(pattern, p); match {
				return true
			}
			for _, part := range strings.Split(p, "/") {
				if match, _ := filepath.Match(pattern, part); match {
					return true
				}
			}
		}
		return false
	}
}
func (w *Workspace) scan() (map[string]bool, error) {
	result := map[string]bool{}
	ignored := w.ignoreMatcher()
	err := filepath.WalkDir(w.Root, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filename == w.Root {
			return nil
		}
		p, _ := filepath.Rel(w.Root, filename)
		p = filepath.ToSlash(p)
		if strings.EqualFold(entry.Name(), ".bough") || strings.EqualFold(entry.Name(), ".git") || ignored(p) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relevant := Selected(p, w.State.Prefixes)
		for _, prefix := range w.State.Prefixes {
			if under(prefix, p) {
				relevant = true
			}
		}
		if !relevant {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err = ValidPath(p, false); err != nil {
			return err
		}
		if !entry.IsDir() {
			result[p] = true
		}
		return nil
	})
	return result, err
}
func (w *Workspace) Status() (map[string]string, error) {
	if result, err := w.monitoredStatus(); err == nil {
		return result, nil
	}
	return w.fullStatus()
}
func (w *Workspace) fullStatus() (map[string]string, error) {
	result := map[string]string{}
	for p, old := range w.State.Files {
		f, _, err := ReadLocal(w.Root, p)
		if err != nil {
			return nil, err
		}
		if f == nil {
			result[p] = "D"
		} else if *f != old {
			result[p] = "M"
		}
	}
	all, err := w.scan()
	if err != nil {
		return nil, err
	}
	staged := map[string]bool{}
	for _, p := range w.State.Staged {
		staged[p] = true
	}
	for p := range all {
		if _, tracked := w.State.Files[p]; !tracked {
			result[p] = "?"
			if staged[p] {
				result[p] = "A"
			}
		}
	}
	return result, nil
}
func (w *Workspace) Add(paths []string) error {
	if w.State.Pending != nil {
		return fail(409, "resume the pending commit first")
	}
	available, err := w.scan()
	if err != nil {
		return err
	}
	selected := map[string]bool{}
	for _, p := range w.State.Staged {
		selected[p] = true
	}
	for _, p := range paths {
		if p == "." {
			p = ""
		} else {
			p = strings.TrimSuffix(p, "/")
			if err = ValidPath(p, false); err != nil {
				return err
			}
		}
		found := false
		for filename := range available {
			if under(filename, p) {
				found = true
				if _, tracked := w.State.Files[filename]; !tracked {
					selected[filename] = true
				}
			}
		}
		if !found && p != "" {
			return fail(400, "path is absent, ignored or outside the sparse view", p)
		}
	}
	w.State.Staged = []string{}
	for p := range selected {
		w.State.Staged = append(w.State.Staged, p)
	}
	sort.Strings(w.State.Staged)
	return w.Save()
}
func (w *Workspace) Commit(ctx context.Context, message, mergeParent string) (string, error) {
	if w.State.Pending == nil {
		paths := map[string]bool{}
		for p := range w.State.Files {
			paths[p] = true
		}
		for _, p := range w.State.Staged {
			paths[p] = true
		}
		changes := map[string]*File{}
		files := map[string]File{}
		for p, f := range w.State.Files {
			files[p] = f
		}
		for p := range paths {
			current, data, err := ReadLocal(w.Root, p)
			if err != nil {
				return "", err
			}
			old, ok := w.State.Files[p]
			if current == nil && !ok || current != nil && ok && *current == old {
				continue
			}
			if current != nil {
				var response struct {
					ID string `json:"id"`
				}
				if err = w.Client.Request(ctx, "POST", w.Client.route("blobs"), data, &response); err != nil {
					return "", err
				}
				if response.ID != current.ID {
					return "", fail(502, "upload identity mismatch")
				}
				files[p] = *current
			} else {
				delete(files, p)
			}
			changes[p] = current
		}
		if len(changes) == 0 && mergeParent == "" {
			return "", fail(400, "no tracked or staged changes")
		}
		parents := []string{w.State.Revision}
		if mergeParent != "" {
			id, err := w.Client.Resolve(ctx, mergeParent)
			if err != nil {
				return "", err
			}
			parents = append(parents, id)
		}
		w.State.Pending = &PendingCommit{Request: CommitRequest{Parents: parents, Changes: changes, Message: message, RequestID: RequestID()}, Files: files}
		if err := w.Save(); err != nil {
			return "", err
		}
	}
	var output struct {
		ID string `json:"id"`
	}
	if err := w.Client.Request(ctx, "POST", w.Client.route("commits"), w.State.Pending.Request, &output); err != nil {
		return "", err
	}
	w.State.Revision = output.ID
	w.State.Files = w.State.Pending.Files
	w.State.Pending = nil
	w.State.Staged = []string{}
	return output.ID, w.Save()
}

type recovery struct {
	Operation string         `json:"operation"`
	Before    WorkspaceState `json:"before"`
	After     WorkspaceState `json:"after"`
	Paths     []string       `json:"paths"`
}

func (w *Workspace) Update(ctx context.Context, revision string) (string, error) {
	if w.State.Pending != nil || len(w.State.Staged) > 0 {
		return "", fail(409, "commit staged or pending work before updating")
	}
	status, err := w.Status()
	if err != nil {
		return "", err
	}
	for _, s := range status {
		if s != "?" {
			return "", fail(409, "commit local changes before updating")
		}
	}
	checkout, err := w.Client.Checkout(ctx, revision)
	if err != nil {
		return "", err
	}
	files, err := w.Client.Manifest(ctx, checkout, w.State.Prefixes)
	if err != nil {
		return "", err
	}
	changed := map[string]bool{}
	for p, f := range files {
		if old, ok := w.State.Files[p]; !ok || old != f {
			changed[p] = true
		}
	}
	for p := range w.State.Files {
		if _, ok := files[p]; !ok {
			changed[p] = true
		}
	}
	for p := range files {
		name, err := SafePath(w.Root, p)
		if err != nil {
			return "", err
		}
		if info, err := os.Lstat(name); err == nil {
			if !info.IsDir() {
				if _, tracked := w.State.Files[p]; !tracked {
					return "", fail(409, "untracked content blocks update", p)
				}
			} else {
				err = filepath.WalkDir(name, func(n string, d fs.DirEntry, e error) error {
					if e != nil {
						return e
					}
					if !d.IsDir() {
						rel, _ := filepath.Rel(w.Root, n)
						if _, tracked := w.State.Files[filepath.ToSlash(rel)]; !tracked {
							return fail(409, "untracked content blocks update", rel)
						}
					}
					return nil
				})
				if err != nil {
					return "", err
				}
			}
		}
		for parent := filepath.Dir(name); parent != w.Root && strings.HasPrefix(parent, w.Root+string(os.PathSeparator)); parent = filepath.Dir(parent) {
			if info, err := os.Lstat(parent); err == nil && !info.IsDir() {
				rel, _ := filepath.Rel(w.Root, parent)
				if _, tracked := w.State.Files[filepath.ToSlash(rel)]; !tracked {
					return "", fail(409, "untracked ancestor blocks update", rel)
				}
			}
		}
	}
	op := RequestID()
	stage := filepath.Join(w.Root, ".bough", "stage", op)
	backup := filepath.Join(w.Root, ".bough", "backups", op)
	paths := []string{}
	for p := range changed {
		paths = append(paths, p)
		if f, exists := files[p]; exists {
			data, err := w.Client.Object(ctx, checkout, "blob", f.ID, p)
			if err != nil {
				return "", err
			}
			if err = WriteLocal(stage, p, f, data); err != nil {
				return "", err
			}
		}
		if old, exists := w.State.Files[p]; exists {
			f, data, err := ReadLocal(w.Root, p)
			if err != nil {
				return "", err
			}
			if f == nil || *f != old {
				return "", fail(409, "file changed during update preparation", p)
			}
			if err = WriteLocal(filepath.Join(backup, "files"), p, *f, data); err != nil {
				return "", err
			}
		}
	}
	sort.Strings(paths)
	status, err = w.Status()
	if err != nil {
		return "", err
	}
	for _, s := range status {
		if s != "?" {
			return "", fail(409, "file changed during update preparation")
		}
	}
	after := w.State
	after.Revision = checkout.Revision
	after.Files = files
	journal := recovery{Operation: op, Before: w.State, After: after, Paths: paths}
	if err = AtomicFile(filepath.Join(backup, "state.json"), Encode(w.State), 0600); err != nil {
		return "", err
	}
	journalPath := filepath.Join(w.Root, ".bough", "recovery.json")
	if err = AtomicFile(journalPath, Encode(journal), 0600); err != nil {
		return "", err
	}
	if err = w.displace(paths, w.State.Files, filepath.Join(backup, "displaced")); err != nil {
		return "", err
	}
	for _, p := range paths {
		if f, ok := files[p]; ok {
			_, data, err := ReadLocal(stage, p)
			if err != nil {
				return "", err
			}
			if err = WriteLocal(w.Root, p, f, data); err != nil {
				return "", err
			}
		}
	}
	w.State = after
	if err = w.Save(); err != nil {
		return "", err
	}
	if err = os.Remove(journalPath); err != nil {
		return "", err
	}
	_ = os.RemoveAll(stage)
	return after.Revision, nil
}
func (w *Workspace) Recover() (string, error) {
	journalPath := filepath.Join(w.Root, ".bough", "recovery.json")
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		return "", err
	}
	var journal recovery
	if err = jsonDecode(raw, &journal); err != nil {
		return "", err
	}
	if err = ValidID(journal.Operation); err != nil && len(journal.Operation) != 48 {
		return "", fail(400, "invalid recovery operation")
	}
	if _, err := hex.DecodeString(journal.Operation); err != nil {
		return "", fail(400, "invalid recovery operation")
	}
	backupRoot := filepath.Join(w.Root, ".bough", "backups", journal.Operation)
	backup := filepath.Join(backupRoot, "files")
	// Verify every snapshot before any mutation, including directory/file transitions.
	for _, p := range journal.Paths {
		if err = ValidPath(p, false); err != nil {
			return "", err
		}
		if old, ok := journal.Before.Files[p]; ok {
			saved, _, err := ReadLocal(backup, p)
			if err != nil || saved == nil || *saved != old {
				return "", fail(500, "recovery snapshot is missing or corrupt", p)
			}
			displaced, _, err := ReadLocal(filepath.Join(backupRoot, "displaced"), p)
			if err != nil {
				return "", err
			}
			if displaced != nil && *displaced != old {
				return "", fail(409, "concurrent edit preserved; copy it aside before recovery", filepath.Join(backupRoot, "displaced", p))
			}
		}
	}
	current := map[string]File{}
	allowed := map[string]bool{}
	for _, p := range journal.Paths {
		allowed[p] = true
	}
	for _, p := range journal.Paths {
		name, err := SafePath(w.Root, p)
		if err != nil {
			return "", err
		}
		info, err := os.Lstat(name)
		if err == nil && info.IsDir() {
			err = filepath.WalkDir(name, func(n string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				rel, _ := filepath.Rel(w.Root, n)
				if !allowed[filepath.ToSlash(rel)] {
					return fail(409, "unknown descendant blocks recovery", rel)
				}
				return nil
			})
			if err != nil {
				return "", err
			}
			continue
		}
		f, _, err := ReadLocal(w.Root, p)
		if err != nil {
			return "", err
		}
		old, oldOK := journal.Before.Files[p]
		new, newOK := journal.After.Files[p]
		if f != nil {
			if (!oldOK || *f != old) && (!newOK || *f != new) {
				return "", fail(409, "later edit found; copy it aside before recovery", p)
			}
			current[p] = *f
		}
	}
	if err = w.displace(journal.Paths, current, filepath.Join(backupRoot, "recover-"+RequestID())); err != nil {
		return "", err
	}
	for _, p := range journal.Paths {
		if old, ok := journal.Before.Files[p]; ok {
			_, data, err := ReadLocal(backup, p)
			if err != nil {
				return "", err
			}
			if err = WriteLocal(w.Root, p, old, data); err != nil {
				return "", err
			}
		}
	}
	w.State = journal.Before
	if err = w.Save(); err != nil {
		return "", err
	}
	if err = os.Remove(journalPath); err != nil {
		return "", err
	}
	return w.State.Revision, syncDir(filepath.Dir(journalPath))
}

// Rename the actual tracked inode into a durable backup. An editor holding it
// open can continue writing without losing those writes. Never unlink new paths.
func (w *Workspace) displace(paths []string, expected map[string]File, destination string) error {
	directory, err := os.OpenRoot(w.Root)
	if err != nil {
		return err
	}
	defer directory.Close()
	ordered := append([]string{}, paths...)
	sort.Slice(ordered, func(i, j int) bool { return strings.Count(ordered[i], "/") > strings.Count(ordered[j], "/") })
	for _, p := range ordered {
		old, ok := expected[p]
		if !ok {
			continue
		}
		name, err := SafePath(w.Root, p)
		if err != nil {
			return err
		}
		target, err := SafePath(destination, p)
		if err != nil {
			return err
		}
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			return fail(409, "backup path already exists", target)
		}
		relativeTarget, err := filepath.Rel(w.Root, target)
		if err != nil || strings.HasPrefix(relativeTarget, "..") {
			return fail(400, "backup must be inside the workspace")
		}
		if err = directory.Rename(p, relativeTarget); err != nil {
			return err
		}
		if err = syncDir(filepath.Dir(target)); err != nil {
			return err
		}
		if err = syncDir(filepath.Dir(name)); err != nil {
			return err
		}
		actual, _, err := ReadLocal(destination, p)
		if err != nil {
			return err
		}
		if actual == nil || *actual != old {
			return fail(409, "concurrent edit preserved; run bough recover after copying it aside", target)
		}
		for parent := filepath.Dir(p); parent != "."; parent = filepath.Dir(parent) {
			if removeEmptyDirectory(directory, parent) != nil {
				break
			}
		}

	}
	return nil
}
