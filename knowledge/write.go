package knowledge

import (
	"context"
	"crypto/sha1" //nolint:gosec // git blob IDs are SHA-1
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/library/index"
	"github.com/xaroth/speki/vault"
)

// Result is what a change did.
type Result struct {
	Version      string // the document's new version; empty after a delete
	Commit       string
	IndexPending bool     // the index hadn't caught up with the commit yet
	LinkedFrom   []string // documents linking to the old path, after a move or delete
}

// WriteRequest creates a document, or replaces one entirely.
type WriteRequest struct {
	Vault   string
	Path    string
	Content string
	Version string // the version being replaced; empty creates
	Message string
}

// EditRequest replaces exact text in a document.
type EditRequest struct {
	Vault      string
	Path       string
	OldText    string
	NewText    string
	ReplaceAll bool
	Version    string // optional
	Message    string
}

// MoveRequest moves a document to another path.
type MoveRequest struct {
	Vault   string
	From    string
	To      string
	Version string // of From
	Message string
}

// DeleteRequest deletes a document.
type DeleteRequest struct {
	Vault   string
	Path    string
	Version string
	Message string
}

// Write creates a document (without a version) or replaces one (with the version being replaced).
func (b *Base) Write(ctx context.Context, g *access.Grant, req WriteRequest) (Result, error) {
	path := strings.TrimPrefix(req.Path, "/")
	return b.change(ctx, g, req.Vault, req.Message, func(ctx context.Context, st state) (plan, error) {
		cur, exists, err := st.document(ctx, path)
		if err != nil {
			return plan{}, err
		}
		if exists && cur.Status == index.StatusBroken {
			return plan{}, brokenDocument(cur)
		}
		if req.Version == "" {
			if exists {
				return plan{}, &Error{
					Code:    CodeAlreadyExists,
					Message: fmt.Sprintf("%s already exists, at version %s. Read it and pass that version to replace it, or use edit to change part of it", cur.Path, cur.Version),
					Version: cur.Version,
				}
			}
			newPath, err := st.newPath(path, "")
			if err != nil {
				return plan{}, err
			}
			out, err := st.prepareDocument(ctx, newPath, nil, []byte(req.Content), b.now())
			if err != nil {
				return plan{}, err
			}
			return plan{
				changes: []gh.FileChange{{Path: newPath, Content: out}},
				message: "Create " + newPath,
				path:    newPath,
				version: blobID(out),
			}, nil
		}

		if !exists {
			return plan{}, errorf(CodeNotFound, "%s has no document %s. Leave out version to create it", st.vault.Name, path)
		}
		if cur.Version != req.Version {
			return plan{}, stale(cur)
		}
		raw, err := st.copy.Blob(cur.Version)
		if err != nil {
			return plan{}, err
		}
		out, err := st.prepareDocument(ctx, cur.Path, raw, []byte(req.Content), b.now())
		if err != nil {
			return plan{}, err
		}
		return plan{
			changes: []gh.FileChange{{Path: cur.Path, Content: out}},
			message: "Update " + cur.Path,
			path:    cur.Path,
			version: blobID(out),
			base:    cur,
		}, nil
	})
}

// Edit replaces OldText in a document, once unless ReplaceAll is set.
func (b *Base) Edit(ctx context.Context, g *access.Grant, req EditRequest) (Result, error) {
	if _, err := writable(g, req.Vault); err != nil {
		return Result{}, err
	}
	if req.OldText == "" {
		return Result{}, errorf(CodeInvalid, "old_text is empty. Give the exact text to replace")
	}
	path := strings.TrimPrefix(req.Path, "/")
	return b.change(ctx, g, req.Vault, req.Message, func(ctx context.Context, st state) (plan, error) {
		cur, exists, err := st.document(ctx, path)
		if err != nil {
			return plan{}, err
		}
		if !exists {
			return plan{}, errorf(CodeNotFound, "%s has no document %s. Use search or list to find it", st.vault.Name, path)
		}
		if cur.Status == index.StatusBroken {
			return plan{}, brokenDocument(cur)
		}
		if req.Version != "" && cur.Version != req.Version {
			return plan{}, stale(cur)
		}
		raw, err := st.copy.Blob(cur.Version)
		if err != nil {
			return plan{}, err
		}
		text := string(raw)
		n := countOverlapping(text, req.OldText)
		switch {
		case n == 0:
			return plan{}, errorf(CodeInvalid, "old_text doesn't appear in %s. Read it again and copy the text exactly; whitespace and line endings count", cur.Path)
		case n > 1 && !req.ReplaceAll:
			return plan{}, errorf(CodeInvalid, "old_text appears %d times in %s. Include more of the surrounding text so it matches once, or set replace_all", n, cur.Path)
		}
		limit := 1
		if req.ReplaceAll {
			limit = -1
		}
		next := strings.Replace(text, req.OldText, req.NewText, limit)
		out, err := st.prepareDocument(ctx, cur.Path, raw, []byte(next), b.now())
		if err != nil {
			return plan{}, err
		}
		return plan{
			changes: []gh.FileChange{{Path: cur.Path, Content: out}},
			message: "Update " + cur.Path,
			path:    cur.Path,
			version: blobID(out),
			base:    cur,
		}, nil
	})
}

// Move moves a document in one commit. Links to it elsewhere aren't changed: the result lists the
// documents that link to the old path.
func (b *Base) Move(ctx context.Context, g *access.Grant, req MoveRequest) (Result, error) {
	if _, err := writable(g, req.Vault); err != nil {
		return Result{}, err
	}
	if req.Version == "" {
		return Result{}, errorf(CodeInvalid, "move needs the version of the document being moved. Read it first")
	}
	from := strings.TrimPrefix(req.From, "/")
	to := strings.TrimPrefix(req.To, "/")
	if from == to {
		return Result{}, errorf(CodeInvalid, "from and to are the same path")
	}
	return b.change(ctx, g, req.Vault, req.Message, func(ctx context.Context, st state) (plan, error) {
		cur, exists, err := st.document(ctx, from)
		if err != nil {
			return plan{}, err
		}
		if !exists {
			return plan{}, errorf(CodeNotFound, "%s has no document %s. Use search or list to find it", st.vault.Name, from)
		}
		if cur.Status == index.StatusBroken {
			return plan{}, brokenDocument(cur)
		}
		if cur.Version != req.Version {
			return plan{}, stale(cur)
		}
		target, exists, err := st.document(ctx, to)
		if err != nil {
			return plan{}, err
		}
		if exists {
			return plan{}, &Error{
				Code:    CodeAlreadyExists,
				Message: fmt.Sprintf("%s already exists, at version %s. Pick another path, or delete that document first", target.Path, target.Version),
				Version: target.Version,
			}
		}
		newPath, err := st.newPath(to, cur.Path)
		if err != nil {
			return plan{}, err
		}
		if newPath == cur.Path { // to was the same path in another normalisation
			return plan{}, errorf(CodeInvalid, "%s and %s are the same path", from, to)
		}
		raw, err := st.copy.Blob(cur.Version)
		if err != nil {
			return plan{}, err
		}
		linked, err := st.copy.Index().LinkedFrom(ctx, cur.Path)
		if err != nil {
			return plan{}, err
		}
		return plan{
			changes:    []gh.FileChange{{Path: cur.Path}, {Path: newPath, Content: raw}},
			message:    "Move " + cur.Path + " to " + newPath,
			path:       newPath,
			source:     cur.Path,
			version:    cur.Version,
			linkedFrom: linked,
			base:       cur,
		}, nil
	})
}

// Delete deletes a document. It stays in git history.
func (b *Base) Delete(ctx context.Context, g *access.Grant, req DeleteRequest) (Result, error) {
	if _, err := writable(g, req.Vault); err != nil {
		return Result{}, err
	}
	if req.Version == "" {
		return Result{}, errorf(CodeInvalid, "delete needs the version of the document. Read it first")
	}
	path := strings.TrimPrefix(req.Path, "/")
	return b.change(ctx, g, req.Vault, req.Message, func(ctx context.Context, st state) (plan, error) {
		cur, exists, err := st.document(ctx, path)
		if err != nil {
			return plan{}, err
		}
		if !exists {
			return plan{}, errorf(CodeNotFound, "%s has no document %s. Use search or list to find it", st.vault.Name, path)
		}
		if cur.Version != req.Version {
			return plan{}, stale(cur)
		}
		linked, err := st.copy.Index().LinkedFrom(ctx, cur.Path)
		if err != nil {
			return plan{}, err
		}
		return plan{
			changes:    []gh.FileChange{{Path: cur.Path}},
			message:    "Delete " + cur.Path,
			path:       cur.Path,
			linkedFrom: linked,
			base:       cur,
		}, nil
	})
}

// writable returns a vault the user may change, or why not. Every change checks this first, so the
// answer doesn't depend on whether the rest of the request is well formed.
func writable(g *access.Grant, name string) (vault.Vault, error) {
	v, ok := g.Vault(name)
	switch {
	case !ok:
		return vault.Vault{}, missingVault(g, name)
	case g.ReadOnlySession():
		return vault.Vault{}, errorf(CodeReadOnlySession, "the user connected speki read-only. Ask them to reconnect it with write access")
	case !g.CanWrite(v.RepoID):
		return vault.Vault{}, errorf(CodeNoWriteAccess, "the user can read %s but can't push to it on GitHub. An owner of the repository can give them write access", v.Name)
	}
	return v, nil
}

// countOverlapping counts the places sub appears in s, overlapping ones too: "ab ab" appears twice in
// "ab ab ab", and an edit of it would be ambiguous.
func countOverlapping(s, sub string) int {
	n := 0
	for i := 0; ; i++ {
		j := strings.Index(s[i:], sub)
		if j < 0 {
			return n
		}
		n++
		i += j
	}
}

// blobID returns the git blob SHA of content: its version.
func blobID(content []byte) string {
	h := sha1.New() //nolint:gosec // git blob IDs are SHA-1
	h.Write([]byte("blob " + strconv.Itoa(len(content)) + "\x00"))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}
