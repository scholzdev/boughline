package boughline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	ProtocolVersion = 1
	MaxRequest      = 8 << 20
	MaxBlob         = 256 << 20
	MaxChanges      = 10000
	Regular         = uint32(0100644)
	Executable      = uint32(0100755)
	Symlink         = uint32(0120000)
)

type Fault struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
	Details any    `json:"details,omitempty"`
}

func (e *Fault) Error() string { return e.Message }
func fail(status int, message string, details ...any) error {
	var d any
	if len(details) > 0 {
		d = details[0]
	}
	return &Fault{Status: status, Message: message, Details: d}
}
func statusOf(err error) int {
	var f *Fault
	if errors.As(err, &f) {
		return f.Status
	}
	return 500
}

func Hash(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func Encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func Decode(b []byte, v any) error { return jsonDecode(b, v) }
func jsonDecode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
func sortStrings(s []string) { sort.Strings(s) }

var idPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func ValidID(id string) error {
	if !idPattern.MatchString(id) {
		return fail(400, "invalid object ID")
	}
	return nil
}
func ValidName(s string) error {
	if !namePattern.MatchString(s) {
		return fail(400, "invalid name")
	}
	return nil
}
func ValidPath(p string, root bool) error {
	if root && p == "" {
		return nil
	}
	if p == "" || len(p) > 4096 || !utf8.ValidString(p) || path.Clean(p) != p || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\:\x00") || len(strings.Split(p, "/")) > 128 {
		return fail(400, "invalid repository path", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." || len(part) > 255 || strings.EqualFold(part, ".bough") || strings.EqualFold(part, ".git") || strings.IndexFunc(part, unicode.IsControl) >= 0 {
			return fail(400, "unsafe repository path", p)
		}
	}
	return nil
}
func ValidText(s string, max int) error {
	if strings.TrimSpace(s) == "" || len(s) > max || !utf8.ValidString(s) {
		return fail(400, "invalid text")
	}
	return nil
}

type File struct {
	ID   string `json:"id"`
	Mode uint32 `json:"mode"`
	Size int64  `json:"size"`
}

func (f File) Validate() error {
	if f.Size < 0 || f.Size > MaxBlob {
		return fail(400, "invalid file size")
	}
	if err := ValidID(f.ID); err != nil {
		return err
	}
	if f.Mode == Symlink && (f.Size == 0 || f.Size > 4096) {
		return fail(400, "invalid symlink size")
	}
	if f.Mode != Regular && f.Mode != Executable && f.Mode != Symlink {
		return fail(400, "unsupported file mode")
	}
	return nil
}
func sameFile(a, b *File) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

type Entry struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Mode  uint32 `json:"mode,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Items int64  `json:"items,omitempty"`
	Count int64  `json:"count,omitempty"`
}
type Tree map[string]Entry
type Changeset struct {
	OriginalGit *OriginalGit     `json:"original_git,omitempty"`
	Version     int              `json:"version"`
	Parents     []string         `json:"parents"`
	Changes     map[string]*File `json:"changes"`
	Author      string           `json:"author"`
	Message     string           `json:"message"`
	TimeNS      int64            `json:"time_ns"`
	RebasedFrom string           `json:"rebased_from,omitempty"`
}
type CommitRequest struct {
	OriginalGit *OriginalGit     `json:"original_git,omitempty"`
	Parents     []string         `json:"parents"`
	Changes     map[string]*File `json:"changes"`
	Message     string           `json:"message"`
	RequestID   string           `json:"request_id"`
	RebasedFrom string           `json:"rebased_from,omitempty"`
}
type Revision struct {
	ID        string    `json:"id"`
	Changeset Changeset `json:"changeset"`
	Root      string    `json:"root,omitempty"`
}
type Graph struct {
	Ancestors  string   `json:"ancestors,omitempty"`
	Generation int64    `json:"generation"`
	Depth      int64    `json:"depth"`
	Jumps      []string `json:"jumps"`
}
type Difference struct {
	Before *File `json:"before"`
	After  *File `json:"after"`
}
type HistoryNode struct {
	Commit   string `json:"commit"`
	Previous string `json:"previous,omitempty"`
	File     *File  `json:"file"`
}
type Origin struct {
	Commit string `json:"commit"`
	Line   int    `json:"line"`
}
type Blame struct {
	Blob    string   `json:"blob"`
	Origins []Origin `json:"origins"`
}

type Rule struct {
	Prefix string `json:"prefix"`
	Read   bool   `json:"read"`
	Write  bool   `json:"write"`
}
type Principal struct {
	Name  string `json:"name"`
	Admin bool   `json:"admin"`
	Rules []Rule `json:"rules"`
}

func Allowed(rules []Rule, p string, write bool) bool {
	best := -1
	result := false
	for _, r := range rules {
		if r.Prefix == "" || p == r.Prefix || strings.HasPrefix(p, r.Prefix+"/") {
			if len(r.Prefix) >= best {
				best = len(r.Prefix)
				result = r.Read
				if write {
					result = r.Write
				}
			}
		}
	}
	return result
}
func (p Principal) Check(filename string, write bool) error {
	if p.Admin || Allowed(p.Rules, filename, write) {
		return nil
	}
	return fail(403, "path access denied", filename)
}

type ReviewPolicy struct {
	Prefix            string   `json:"prefix"`
	Approvals         int      `json:"approvals"`
	Reviewers         []string `json:"reviewers,omitempty"`
	AllowSelf         bool     `json:"allow_self"`
	CommentOnlyExempt bool     `json:"comment_only_exempt"`
}
type RepoConfig struct {
	ForgejoOnly bool           `json:"forgejo_only,omitempty"`
	ID          string         `json:"id"`
	Owner       string         `json:"owner"`
	Policies    []ReviewPolicy `json:"policies"`
}
type ObjectRequest struct {
	Capability string `json:"capability"`
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Path       string `json:"path"`
	Page       string `json:"page,omitempty"`
}
type Checkout struct {
	Repo       string    `json:"repo"`
	Revision   string    `json:"revision"`
	Root       string    `json:"root"`
	Capability string    `json:"capability"`
	Expires    time.Time `json:"expires"`
}

func objectKey(kind, id string) (string, error) {
	if err := ValidID(id); err != nil {
		return "", err
	}
	switch kind {
	case "blob", "tree", "changeset", "history", "blame", "graph", "path":
	default:
		return "", fail(400, "invalid object kind")
	}
	return fmt.Sprintf("%s/%s/%s", kind, id[:2], id[2:]), nil
}
