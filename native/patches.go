package boughline

import (
	"context"
	"github.com/pmezard/go-difflib/difflib"
	"strings"
)

func (e *Engine) Patches(ctx context.Context, changes map[string]Difference) (map[string]string, error) {
	out := map[string]string{}
	budget := 0
	for path, delta := range changes {
		content := func(file *File) (string, error) {
			if file == nil {
				return "", nil
			}
			if file.Size > 1<<20 {
				return "", fail(413, "file exceeds web diff limit")
			}
			raw, err := GetRaw(ctx, e.Objects, "blob", file.ID)
			if err != nil {
				return "", err
			}
			if strings.ContainsRune(string(raw), 0) {
				return "", fail(422, "binary file")
			}
			return string(raw), nil
		}
		a, ea := content(delta.Before)
		b, eb := content(delta.After)
		if ea != nil || eb != nil {
			if ea != nil && statusOf(ea) != 413 && statusOf(ea) != 422 {
				return nil, ea
			}
			if eb != nil && statusOf(eb) != 413 && statusOf(eb) != 422 {
				return nil, eb
			}
			out[path] = "Binary or large file; inspect its content with the bough client."
			continue
		}
		patch, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: difflib.SplitLines(a), B: difflib.SplitLines(b), FromFile: "a/" + path, ToFile: "b/" + path, Context: 3})
		if err != nil {
			return nil, err
		}
		budget += len(patch)
		if budget > 4<<20 {
			return nil, fail(413, "web diff exceeds 4 MiB; narrow the change")
		}
		out[path] = patch
	}
	return out, nil
}
