package knowledge

import (
	"context"
	"strings"

	"github.com/eve-online-tools/speki/access"
	"github.com/eve-online-tools/speki/index"
)

// listLimit is the most entries one List call returns.
const listLimit = 200

// VaultInfo describes a vault the user may read.
type VaultInfo struct {
	Name        string
	Title       string
	Description string
	Guidelines  string
	CanWrite    bool
	Documents   int
}

// Vaults returns the vaults the user may read and speki serves, sorted by name.
func (b *Base) Vaults(ctx context.Context, g *access.Grant) ([]VaultInfo, error) {
	var out []VaultInfo
	for _, v := range g.Readable() {
		c, ok := b.copies(v.RepoID)
		if !ok {
			continue
		}
		stats, err := c.Index().Stats(ctx)
		if err != nil {
			b.logger.WarnContext(ctx, "vault stats failed", "vault", v.Name, "err", err)
			continue
		}
		out = append(out, VaultInfo{
			Name:        v.Name,
			Title:       v.Config.Title,
			Description: v.Config.Description,
			Guidelines:  v.Config.Guidelines,
			CanWrite:    g.CanWrite(v.RepoID),
			Documents:   stats.Documents,
		})
	}
	return out, nil
}

// ListRequest asks for the folders and documents under a folder.
type ListRequest struct {
	Vault     string
	Path      string // the folder; "" is the root
	Recursive bool
	Cursor    string // NextCursor of the previous page
}

// Listing is one page of a folder.
type Listing struct {
	Folders    []string      // paths ending in "/"
	Documents  []index.Entry // sorted by path
	NextCursor string        // empty on the last page
	Guidelines string        // the vault's guidelines, on the first page of the root
}

// List returns one page of the folders and documents under a folder, sorted by path.
func (b *Base) List(ctx context.Context, g *access.Grant, req ListRequest) (Listing, error) {
	v, c, err := b.open(g, req.Vault)
	if err != nil {
		return Listing{}, err
	}
	folder := strings.Trim(req.Path, "/")
	if folder != "" {
		folder += "/"
	}
	if req.Cursor != "" && !strings.HasPrefix(req.Cursor, folder) {
		return Listing{}, errorf(Invalid, "the cursor belongs to another listing. Leave it out to start from the beginning")
	}
	entries, err := c.Index().List(ctx, folder, req.Recursive, req.Cursor, listLimit+1)
	if err != nil {
		return Listing{}, indexError(v.Name, err)
	}
	if len(entries) == 0 && folder != "" && req.Cursor == "" {
		return Listing{}, errorf(NotFound, "%s has no documents under %s. Call list on the vault root to see its folders", v.Name, folder)
	}

	var out Listing
	if len(entries) > listLimit {
		entries = entries[:listLimit]
		out.NextCursor = entries[listLimit-1].Path
	}
	for _, e := range entries {
		if e.IsFolder() {
			out.Folders = append(out.Folders, e.Path)
		} else {
			out.Documents = append(out.Documents, e)
		}
	}
	if folder == "" && req.Cursor == "" {
		out.Guidelines = v.Config.Guidelines
	}
	return out, nil
}
