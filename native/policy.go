package boughline

import (
	"bytes"
	"context"
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"time"
)

func ValidatePolicies(p []ReviewPolicy) error {
	if len(p) > 1000 {
		return fail(400, "too many policies")
	}
	for _, r := range p {
		if err := ValidPath(r.Prefix, true); err != nil {
			return err
		}
		if r.Approvals < 0 || r.Approvals > 20 {
			return fail(400, "invalid approval count")
		}
		for _, name := range r.Reviewers {
			if err := ValidText(name, 128); err != nil {
				return err
			}
		}
	}
	return nil
}
func ValidateRules(r []Rule) error {
	if len(r) > 1000 {
		return fail(400, "too many path grants")
	}
	seen := map[string]bool{}
	for _, p := range r {
		if err := ValidPath(p.Prefix, true); err != nil {
			return err
		}
		if seen[p.Prefix] {
			return fail(400, "duplicate path grant")
		}
		seen[p.Prefix] = true
		if p.Write && !p.Read {
			return fail(400, "write grants must include read")
		}
	}
	return nil
}
func ReviewHash(reviews []Review) string {
	copy := append([]Review{}, reviews...)
	sort.Slice(copy, func(i, j int) bool { return copy[i].Reviewer < copy[j].Reviewer })
	return Hash(Encode(copy))
}

type LandGuard struct {
	ReviewerGrants map[string]string
	ConfigHash     string
	ReviewRevision string
	ReviewsHash    string
}

func (e *Engine) Approve(ctx context.Context, repo, revision string, approve bool, p Principal) error {
	c, err := e.Change(ctx, repo, revision)
	if err != nil {
		return err
	}
	if len(c.Parents) > 0 {
		if _, err = e.Diff(ctx, repo, c.Parents[0], revision, p); err != nil {
			return err
		}
	}
	return e.Meta.Review(ctx, repo, Review{Revision: revision, Reviewer: p.Name, Principal: &p, Approve: approve, Time: time.Now().UTC()})
}
func (e *Engine) checkReview(ctx context.Context, repo, head, work, candidate string, p Principal) (LandGuard, error) {
	config, err := e.Meta.Repo(ctx, repo)
	if err != nil {
		return LandGuard{}, err
	}
	reviews, err := e.Meta.Reviews(ctx, repo, work)
	if err != nil {
		return LandGuard{}, err
	}
	grantHashes := map[string]string{}
	currentGrants := map[string]*Principal{}
	for _, review := range reviews {
		if !review.Approve {
			continue
		}
		grant, err := e.Meta.Grant(ctx, repo, review.Reviewer)
		if err != nil && statusOf(err) != 404 {
			return LandGuard{}, err
		}
		if err == nil {
			grantHashes[review.Reviewer] = Hash(Encode(grant))
			currentGrants[review.Reviewer] = &grant
		} else {
			grantHashes[review.Reviewer] = ""
		}
	}
	actual, err := e.Diff(ctx, repo, head, candidate, Principal{Admin: true})
	if err != nil {
		return LandGuard{}, err
	}
	for filename, diff := range actual {
		if err = p.Check(filename, true); err != nil {
			return LandGuard{}, fail(403, "landing includes paths outside your write permissions")
		}
		for _, rule := range config.Policies {
			if !under(filename, rule.Prefix) || rule.Approvals == 0 {
				continue
			}
			if rule.CommentOnlyExempt && e.commentOnly(ctx, filename, diff) {
				continue
			}
			authors := map[string]bool{}
			if !rule.AllowSelf {
				authors, err = e.pathAuthorsSince(ctx, repo, work, head, filename)
				if err != nil {
					return LandGuard{}, err
				}
			}
			approved := map[string]bool{}
			for _, r := range reviews {
				if !r.Approve || r.Principal == nil || r.Principal.Check(filename, false) != nil || !rule.AllowSelf && authors[r.Reviewer] {
					continue
				}
				if grant := currentGrants[r.Reviewer]; grant != nil && grant.Check(filename, false) != nil {
					continue
				}
				if len(rule.Reviewers) > 0 {
					found := false
					for _, name := range rule.Reviewers {
						if r.Reviewer == name {
							found = true
						}
					}
					if !found {
						continue
					}
				}
				approved[r.Reviewer] = true
			}
			if len(approved) < rule.Approvals {
				return LandGuard{}, fail(403, "review requirement not satisfied", map[string]any{"path": filename, "required": rule.Approvals, "approved": len(approved), "revision": work})
			}
		}
	}
	return LandGuard{ConfigHash: Hash(Encode(config)), ReviewRevision: work, ReviewsHash: ReviewHash(reviews), ReviewerGrants: grantHashes}, nil
}
func under(p, prefix string) bool {
	return prefix == "" || p == prefix || strings.HasPrefix(p, prefix+"/")
}
func (e *Engine) Land(ctx context.Context, repo, ref, expected, work, request string, p Principal) (string, error) {
	if err := ValidName(ref); err != nil {
		return "", err
	}
	current, err := e.Meta.Ref(ctx, repo, ref)
	if err != nil {
		return "", err
	}
	if current != expected {
		return "", fail(409, "ref changed", map[string]string{"current": current})
	}
	candidate, err := e.Merge(ctx, repo, current, work, "Land "+work+" on "+ref, request, p)
	if err != nil {
		return "", err
	}
	if candidate == current {
		return current, nil
	}
	guard, err := e.checkReview(ctx, repo, current, work, candidate, p)
	if err != nil {
		return "", err
	}
	if yes, err := e.Ancestor(ctx, repo, current, candidate); err != nil {
		return "", err
	} else if !yes {
		return "", fail(409, "ref update must be fast-forward")
	}
	if err = e.Meta.CAS(ctx, repo, ref, current, candidate, guard); err != nil {
		return "", err
	}
	return candidate, nil
}
func (e *Engine) commentOnly(ctx context.Context, filename string, d Difference) bool {
	if !strings.HasSuffix(filename, ".go") || d.Before == nil || d.After == nil || d.Before.Mode != d.After.Mode {
		return false
	}
	a, err := GetRaw(ctx, e.Objects, "blob", d.Before.ID)
	if err != nil {
		return false
	}
	b, err := GetRaw(ctx, e.Objects, "blob", d.After.ID)
	if err != nil {
		return false
	}
	for _, raw := range [][]byte{a, b} {
		for _, directive := range []string{"//go:", "// +build", "//line ", "/*line ", "//export ", "#cgo"} {
			if bytes.Contains(raw, []byte(directive)) {
				return false
			}
		}
	}
	canonicalGo := func(raw []byte) ([]byte, error) {
		set := token.NewFileSet()
		ast, err := parser.ParseFile(set, filename, raw, 0)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		err = format.Node(&buf, set, ast)
		return buf.Bytes(), err
	}
	x, err := canonicalGo(a)
	if err != nil {
		return false
	}
	y, err := canonicalGo(b)
	return err == nil && bytes.Equal(x, y)
}

// A later commit in a stack cannot turn an earlier author's own protected edit
// into somebody else's change for self-approval purposes.
func (e *Engine) pathAuthorsSince(ctx context.Context, repo, work, head, filename string) (map[string]bool, error) {
	root, err := e.Deriver.Get(ctx, repo, work, HistoryKind)
	if err != nil {
		return nil, err
	}
	entry, err := e.lookup(ctx, root, historyPath(filename))
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, fail(500, "missing path history for reviewed change")
	}
	authors := map[string]bool{}
	id := entry.ID
	for id != "" {
		var node HistoryNode
		if err = GetJSON(ctx, e.Objects, "history", id, &node); err != nil {
			return nil, err
		}
		included, err := e.Ancestor(ctx, repo, node.Commit, head)
		if err != nil {
			return nil, err
		}
		if included {
			break
		}
		change, err := e.Change(ctx, repo, node.Commit)
		if err != nil {
			return nil, err
		}
		authors[change.Author] = true
		id = node.Previous
	}
	return authors, nil
}
