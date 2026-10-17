package boughline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// OriginalGit preserves provenance independently of the authenticated submitter.
// Original IDs are informational, never accepted as native content identities.
type OriginalGit struct {
	Raw           []byte `json:"raw"`
	ID            string `json:"id"`
	Author        string `json:"author"`
	Committer     string `json:"committer"`
	AuthorTime    string `json:"author_time"`
	CommitterTime string `json:"committer_time"`
}
type importCheckpoint struct {
	Version                       int `json:"version"`
	Server, Repo, Source, Genesis string
	Commits                       map[string]string
	Refs                          map[string]string
}

func gitRead(ctx context.Context, source string, limit int64, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", source}, args...)...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1")
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	pipe, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = command.Start(); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(pipe, limit+1))
	if err != nil || int64(len(raw)) > limit {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, fail(413, "Git import object or inventory exceeds limit")
	}
	if err = command.Wait(); err != nil {
		return nil, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(diagnostic.String()))
	}
	return raw, nil
}
func gitIdentity(line string) (identity, stamp string) {
	i := strings.LastIndex(line, "> ")
	if i < 0 {
		return line, ""
	}
	return line[:i+1], line[i+2:]
}
func parseGitCommit(id string, raw []byte) (OriginalGit, []string, string, int64, error) {
	provenance := OriginalGit{ID: id, Raw: raw}
	headers, message, ok := strings.Cut(string(raw), "\n\n")
	if !ok {
		return provenance, nil, "", 0, fail(400, "invalid Git commit")
	}
	parents := []string{}
	for _, line := range strings.Split(headers, "\n") {
		if strings.HasPrefix(line, "parent ") {
			parents = append(parents, strings.TrimPrefix(line, "parent "))
		}
		if strings.HasPrefix(line, "author ") {
			provenance.Author, provenance.AuthorTime = gitIdentity(strings.TrimPrefix(line, "author "))
		}
		if strings.HasPrefix(line, "committer ") {
			provenance.Committer, provenance.CommitterTime = gitIdentity(strings.TrimPrefix(line, "committer "))
		}
	}
	seconds, _, _ := strings.Cut(provenance.CommitterTime, " ")
	stamp, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil || stamp > 9223372036 || stamp < -9223372036 {
		return provenance, nil, "", 0, fail(400, "Git commit timestamp exceeds native nanosecond range", id)
	}
	return provenance, parents, message, stamp * 1e9, nil
}

// ImportGit reads the source object database without modifying it. Each source
// commit has a deterministic request key and a durable checkpoint. A restart
// reuses published immutable commits; refs move only after the complete DAG.
func ImportGit(ctx context.Context, c *Client, source, checkpoint, mainRef string) (map[string]string, error) {
	source, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	if mainRef == "" {
		mainRef = "refs/heads/main"
	}
	state := importCheckpoint{Version: 1, Server: c.Server, Repo: c.Repo, Source: source, Commits: map[string]string{}, Refs: map[string]string{}}
	raw, err := os.ReadFile(checkpoint)
	if err == nil {
		if err = jsonDecode(raw, &state); err != nil {
			return nil, err
		}
		if state.Version != 1 || state.Server != c.Server || state.Repo != c.Repo || state.Source != source {
			return nil, fail(409, "checkpoint belongs to a different import")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if state.Genesis == "" {
		state.Genesis, err = c.Resolve(ctx, "main")
		if err != nil {
			return nil, err
		}
		if err = AtomicFile(checkpoint, Encode(state), 0600); err != nil {
			return nil, err
		}
	}
	inventory, err := gitRead(ctx, source, 64<<20, "rev-list", "--topo-order", "--reverse", "--all")
	if err != nil {
		return nil, err
	}
	for _, gitID := range strings.Fields(string(inventory)) {
		if id := state.Commits[gitID]; id != "" {
			var existing struct {
				ID        string    `json:"id"`
				Changeset Changeset `json:"changeset"`
				Complete  bool      `json:"complete"`
			}
			if err = c.Request(ctx, "GET", c.route("changes/"+id), nil, &existing); err != nil {
				return nil, err
			}
			if existing.Changeset.OriginalGit == nil || existing.Changeset.OriginalGit.ID != gitID {
				return nil, fail(409, "checkpoint provenance mismatch", gitID)
			}
			continue
		}
		raw, err := gitRead(ctx, source, 1<<20, "cat-file", "commit", gitID)
		if err != nil {
			return nil, err
		}
		provenance, gitParents, message, _, err := parseGitCommit(gitID, raw)
		if err != nil {
			return nil, err
		}
		parents := []string{}
		for _, parent := range gitParents {
			id := state.Commits[parent]
			if id == "" {
				return nil, fail(409, "Git import parent was not imported", parent)
			}
			parents = append(parents, id)
		}
		if len(parents) == 0 {
			parents = []string{state.Genesis}
		}
		arguments := []string{"diff-tree", "--no-commit-id", "--no-renames", "--raw", "-r", "-z", "--abbrev=64"}
		if len(gitParents) == 0 {
			arguments = append(arguments, "--root", gitID)
		} else {
			arguments = append(arguments, gitParents[0], gitID)
		}
		delta, err := gitRead(ctx, source, MaxRequest, arguments...)
		if err != nil {
			return nil, err
		}
		parts := bytes.Split(delta, []byte{0})
		changes := map[string]*File{}
		for i := 0; i+1 < len(parts); i += 2 {
			if len(parts[i]) == 0 {
				break
			}
			fields := strings.Fields(string(parts[i]))
			if len(fields) != 5 {
				return nil, fail(400, "unsupported Git diff record")
			}
			filename := string(parts[i+1])
			if err = ValidPath(filename, false); err != nil {
				return nil, err
			}
			if fields[4] == "D" {
				changes[filename] = nil
				continue
			}
			mode64, err := strconv.ParseUint(fields[1], 8, 32)
			if err != nil {
				return nil, err
			}
			mode := uint32(mode64)
			if mode != Regular && mode != Executable && mode != Symlink {
				return nil, fail(422, "Git submodules or unsupported modes require migration before import", filename)
			}
			data, err := gitRead(ctx, source, MaxBlob, "cat-file", "blob", fields[3])
			if err != nil {
				return nil, err
			}
			var uploaded struct {
				ID string `json:"id"`
			}
			if err = c.Request(ctx, "POST", c.route("blobs"), data, &uploaded); err != nil {
				return nil, err
			}
			if uploaded.ID != Hash(data) {
				return nil, fail(502, "Git import upload identity mismatch")
			}
			changes[filename] = &File{ID: uploaded.ID, Mode: mode, Size: int64(len(data))}
		}
		var result struct {
			ID string `json:"id"`
		}
		request := CommitRequest{Parents: parents, Changes: changes, Message: message, RequestID: "git-" + gitID, OriginalGit: &provenance}
		if err = c.Request(ctx, "POST", c.route("commits"), request, &result); err != nil {
			return nil, fmt.Errorf("import %s: %w", gitID, err)
		}
		state.Commits[gitID] = result.ID
		if err = AtomicFile(checkpoint, Encode(state), 0600); err != nil {
			return nil, err
		}
	}
	refs, err := gitRead(ctx, source, MaxRequest, "for-each-ref", "--format=%(refname)", "refs/heads/", "refs/tags/")
	if err != nil {
		return nil, err
	}
	for _, ref := range strings.Fields(string(refs)) {
		peeled, err := gitRead(ctx, source, 1024, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return nil, fmt.Errorf("ref %s does not point to a commit: %w", ref, err)
		}
		id := state.Commits[strings.TrimSpace(string(peeled))]
		if id == "" {
			return nil, fail(409, "Git ref was changed during import; rerun", ref)
		}
		name := "git-" + Hash([]byte(ref))
		if ref == mainRef {
			name = "main"
		}
		if err = c.Request(ctx, "POST", c.route("import-ref"), map[string]string{"ref": name, "expected": state.Genesis, "revision": id}, nil); err != nil {
			return nil, err
		}
		state.Refs[ref] = name
		if err = AtomicFile(checkpoint, Encode(state), 0600); err != nil {
			return nil, err
		}
	}
	return state.Refs, nil
}

func (e *Engine) ImportRef(ctx context.Context, repo, ref, expected, revision string, p Principal) error {
	if !p.Admin {
		return fail(403, "administrator required for Git import")
	}
	if err := ValidName(ref); err != nil {
		return err
	}
	if ref != "main" && !strings.HasPrefix(ref, "git-") {
		return fail(400, "invalid import ref")
	}
	target, err := e.Change(ctx, repo, revision)
	if err != nil {
		return err
	}
	if target.OriginalGit == nil {
		return fail(400, "import ref must target imported history")
	}
	current, err := e.Meta.Ref(ctx, repo, ref)
	if err == nil && current == revision {
		return nil
	}
	if err != nil && statusOf(err) != 404 {
		return err
	}
	if ref == "main" {
		genesis, err := e.Change(ctx, repo, expected)
		if err != nil {
			return err
		}
		if len(genesis.Parents) > 0 || len(genesis.Changes) > 0 || genesis.TimeNS != 0 {
			return fail(409, "Git import cannot overwrite existing main history")
		}
		if current != expected {
			return fail(409, "main changed during Git import")
		}
	} else {
		if current != "" {
			return fail(409, "imported ref already points elsewhere")
		}
		expected = ""
	}
	return e.Meta.CAS(ctx, repo, ref, expected, revision)
}
