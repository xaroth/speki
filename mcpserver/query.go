package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/xaroth/speki/auth/access"
	"github.com/xaroth/speki/knowledge"
)

// SchemaInput takes nothing.
type SchemaInput struct{}

// SchemaOutput describes what query can read.
type SchemaOutput struct {
	Schema string `json:"schema"`
}

func (s *Server) schema(context.Context, *access.Grant, SchemaInput) (SchemaOutput, string, error) {
	return SchemaOutput{Schema: schemaText}, schemaText, nil
}

// QueryInput is one SQL query.
type QueryInput struct {
	SQL    string   `json:"sql" jsonschema:"one read-only SQLite SELECT statement; see schema"`
	Vaults []string `json:"vaults,omitempty" jsonschema:"the vaults to query (owner/repo), at most 10. Leave out for all your vaults, if that's 10 or fewer"`
}

// QueryOutput is a query's result.
type QueryOutput struct {
	Columns   []string       `json:"columns"`
	Rows      [][]any        `json:"rows"`
	Truncated bool           `json:"truncated" jsonschema:"true when rows were cut at 200 rows or the size of one answer"`
	Skipped   []SkippedVault `json:"skipped_vaults,omitempty" jsonschema:"vaults you can read that weren't queried because speki can't serve them right now. The rows say nothing about what they hold"`
}

func (s *Server) query(ctx context.Context, g *access.Grant, in QueryInput) (QueryOutput, string, error) {
	res, err := s.kb.Query(ctx, g, knowledge.QueryRequest{SQL: in.SQL, Vaults: in.Vaults})
	if err != nil {
		return QueryOutput{}, "", err
	}
	r := res.Rows
	out := QueryOutput{Columns: r.Columns, Rows: [][]any{}, Truncated: r.Truncated, Skipped: skippedVaults(res.Skipped)}
	if out.Columns == nil {
		out.Columns = []string{}
	}
	var skipped strings.Builder
	writeSkipped(&skipped, "queried", out.Skipped)
	var b strings.Builder
	cells := make([]string, len(out.Columns))
	for i, c := range out.Columns {
		cells[i] = oneLine(c)
	}
	b.WriteString(strings.Join(cells, "\t") + "\n")
	size := encodedSize(b.String()+skipped.String()) + answerOverhead
	for _, row := range r.Rows {
		for i, v := range row {
			if v == nil {
				cells[i] = "NULL"
			} else {
				cells[i] = oneLine(fmt.Sprint(v))
			}
		}
		line := strings.Join(cells, "\t") + "\n"
		size += encodedSize(line) - 2 // the quotes are counted once, above
		if size > maxAnswer {
			out.Truncated = true
			break
		}
		b.WriteString(line)
		out.Rows = append(out.Rows, row)
	}
	fmt.Fprintf(&b, "%d rows", len(out.Rows))
	if out.Truncated {
		b.WriteString(", truncated: add a LIMIT or narrow the query to see the rest")
	}
	b.WriteString("\n")
	b.WriteString(skipped.String())
	return out, b.String(), nil
}
