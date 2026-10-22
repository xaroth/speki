package knowledge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xaroth/speki/document"
	"github.com/xaroth/speki/library/index"
	"github.com/xaroth/speki/vault"
)

// state is a vault as a change sees it: its copy at the head the commit will be based on.
type state struct {
	vault vault.Vault
	copy  Copy
	head  string
}

func (st state) document(ctx context.Context, path string) (index.Document, bool, error) {
	d, err := st.copy.Index().Document(ctx, path)
	if errors.Is(err, index.ErrNotFound) {
		return index.Document{}, false, nil
	}
	if err != nil {
		return index.Document{}, false, indexError(st.vault.Name, err)
	}
	return d, true, nil
}

// unchanged checks that base is at its indexed version in the git tree at head, the commit the change is
// based on. The agent's version was checked against the index, so an index that disagrees with git would
// let the commit overwrite a change nobody saw. One tree lookup.
func (st state) unchanged(base index.Document) error {
	if base.Path == "" {
		return nil
	}
	v, err := st.copy.Version(st.head, base.Path)
	if err != nil {
		return err
	}
	if v != base.Version {
		return stale(index.Document{Path: base.Path, Version: v})
	}
	return nil
}

// newPath checks a path for a new document against the git tree at head, and returns the path to
// write. moving is the path being moved away from, which doesn't count as a collision.
func (st state) newPath(path, moving string) (string, error) {
	paths, err := st.copy.Paths(st.head)
	if err != nil {
		return "", err
	}
	taken := make(map[string]string, len(paths)) // path -> what it is
	folded := make(map[string]string, len(paths))
	for _, p := range paths {
		if p == moving {
			continue
		}
		taken[p] = "a file"
		folded[document.Fold(p)] = p
		for i := strings.LastIndexByte(p, '/'); i > 0; i = strings.LastIndexByte(p[:i], '/') {
			taken[p[:i]] = "a folder"
			folded[document.Fold(p[:i])] = p[:i]
		}
	}
	checked, err := document.CheckPath(path, func(f string) (string, bool) {
		p, ok := folded[f]
		return p, ok
	})
	if errors.Is(err, document.ErrExists) {
		return "", errorf(CodeAlreadyExists, "%s (stored with a different Unicode encoding of the same name). Pick another path", err)
	}
	if err != nil {
		return "", errorf(CodeInvalid, "%s", err)
	}
	for i := strings.IndexByte(checked, '/'); i > 0; i = next(checked, i) {
		if taken[checked[:i]] == "a file" {
			return "", errorf(CodeAlreadyExists, "%s is a file in the vault, so nothing can go under it. Pick another path", checked[:i])
		}
	}
	switch taken[checked] {
	case "a file":
		return "", errorf(CodeAlreadyExists, "%s exists in the vault but isn't a document speki reads (for example over 1 MiB, stored in Git LFS, or a symlink). Pick another path", checked)
	case "a folder":
		return "", errorf(CodeAlreadyExists, "%s is a folder in the vault. Pick another path", checked)
	}
	return checked, nil
}

// next returns the index of the next "/" in p after i, or -1.
func next(p string, i int) int {
	j := strings.IndexByte(p[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

// prepareDocument checks the new content of a document against its current content (nil for a new
// one) and sets its dates. created is speki's: now for a new document, and kept from the current one
// when the agent's content leaves it out.
func (st state) prepareDocument(ctx context.Context, path string, current, next []byte, now time.Time) ([]byte, error) {
	var cur *document.Document
	created := now
	if current != nil {
		var err error
		cur, err = document.Parse(path, current)
		if err != nil {
			// The current file is unusable; valid content replaces it, but mustn't set keys agents can't.
			cur = &document.Document{}
		}
		created = cur.Meta.Created
	}
	d, err := document.Parse(path, next)
	if err != nil {
		return nil, errorf(CodeInvalid, "the content can't be used: %s", err)
	}
	unresolved, err := st.checkRelated(ctx, cur, d)
	if err != nil {
		return nil, err
	}
	err = errors.Join(document.Check(cur, d, st.vault.Config.Frontmatter.Allow), unresolved)
	if err != nil {
		return nil, documentError(err)
	}
	out, err := d.Touch(now, created)
	if err != nil {
		return nil, documentError(err)
	}
	return out, nil
}

// checkRelated returns a problem for each related entry next adds that doesn't point to a document in
// the vault, and err when the index can't answer. Entries current already has are left alone, so deleting
// a document doesn't block its referrers.
func (st state) checkRelated(ctx context.Context, cur, next *document.Document) (problems, err error) {
	var found []error
	for _, l := range document.AddedRelated(cur, next) {
		ok, err := st.copy.Index().Resolves(ctx, l)
		if err != nil {
			return nil, indexError(st.vault.Name, err)
		}
		if !ok {
			found = append(found, fmt.Errorf("related: %q isn't a document in this vault", l.Target))
		}
	}
	return errors.Join(found...), nil
}

func documentError(err error) *Error {
	msg := strings.ReplaceAll(err.Error(), "\n", "; ")
	if errors.Is(err, document.ErrTooLarge) {
		return errorf(CodeTooLarge, "%s. Split the document into smaller ones", msg)
	}
	return errorf(CodeInvalid, "%s", msg)
}

// staleError means the document changed since the version the agent gave.
type staleError struct{ conflict *Error }

func (e *staleError) Error() string { return e.conflict.Error() }

func stale(cur index.Document) error {
	if cur.Version == "" {
		return &staleError{conflict: errorf(CodeConflict, "%s was deleted since you read it. Use search or list to find it", cur.Path)}
	}
	return &staleError{conflict: &Error{
		Code:    CodeConflict,
		Message: fmt.Sprintf("%s changed since you read it. Current version: %s. Read it again and redo your change", cur.Path, cur.Version),
		Version: cur.Version,
	}}
}
