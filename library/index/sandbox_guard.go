// mattn/go-sqlite3 only compiles in FTS5, which the index needs, with this tag.
//go:build sqlite_fts5

package index

import (
	"fmt"
	"strings"
)

// Authorizer actions and answers, from sqlite3.h.
const (
	authOK       = 0
	authDeny     = 1
	actPragma    = 19
	actRead      = 20
	actSelect    = 21
	actFunction  = 31
	actRecursive = 33
)

// readable lists the tables of an index a query may read: the schema's own and the full-text shadow
// tables FTS5 reads while matching. resolved_links is a view over links and documents.
var readable = map[string]bool{
	"documents": true, "tags": true, "links": true, "headings": true, "resolved_links": true,
	"fts": true, "fts_data": true, "fts_idx": true, "fts_config": true, "fts_docsize": true,
}

// views are the temp views setUp creates.
var views = map[string]bool{"documents": true, "tags": true, "links": true, "headings": true, "fts": true}

// functions lists the SQL functions queries may call.
var functions = map[string]bool{}

func init() {
	for f := range strings.FieldsSeq(`abs coalesce ifnull iif if instr length octet_length like glob lower upper ltrim rtrim trim
		max min nullif replace round substr substring typeof unicode char hex unhex quote printf format sign concat concat_ws
		avg count group_concat string_agg sum total
		row_number rank dense_rank percent_rank cume_dist ntile lag lead first_value last_value nth_value
		date time datetime julianday unixepoch strftime timediff
		json json_extract json_each json_tree json_array json_object json_array_length json_type json_valid json_quote
		json_group_array json_group_object -> ->> match`) {
		functions[f] = true
	}
}

// authorize returns the authorizer for a sandbox connection with n vaults attached. mattn's driver
// passes the action and its first three arguments; for reads the third is the database name.
func authorize(n int) func(action int, arg1, arg2, db string) int {
	attached := map[string]bool{}
	for i := range n {
		attached[fmt.Sprintf("v%d", i)] = true
	}
	return func(action int, arg1, arg2, db string) int {
		switch action {
		case actSelect, actRecursive:
			return authOK
		case actRead:
			switch {
			case db == "": // a CTE or subquery
				return authOK
			case db == "temp" && views[arg1]:
				return authOK
			case db == "main" && (arg1 == "json_each" || arg1 == "json_tree"): // table-valued functions
				return authOK
			case attached[db] && readable[arg1]:
				return authOK
			}
		case actFunction:
			if functions[strings.ToLower(arg2)] {
				return authOK
			}
		case actPragma:
			if arg1 == "data_version" && arg2 == "" { // FTS5 checks it before reading
				return authOK
			}
		}
		return authDeny
	}
}

// singleStatement accepts one SELECT (WITH and VALUES included), followed by nothing but whitespace,
// comments and a semicolon.
func singleStatement(query string) error {
	end, seen := -1, false
	for i := 0; i < len(query); i++ {
		c := query[i]
		comment := strings.HasPrefix(query[i:], "--") || strings.HasPrefix(query[i:], "/*")
		if !seen && !isSpace(c) && !comment {
			seen = true
			if !startsWithKeyword(query[i:], "select", "with", "values") {
				return fmt.Errorf("%w: only SELECT statements are allowed", ErrBadQuery)
			}
		}
		if end >= 0 && !isSpace(c) && !comment {
			return fmt.Errorf("%w: only one statement is allowed", ErrBadQuery)
		}
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := strings.IndexByte(query[i+1:], c)
			if j < 0 {
				return nil // unterminated: SQLite reports it
			}
			i += j + 1
		case c == '[':
			j := strings.IndexByte(query[i+1:], ']')
			if j < 0 {
				return nil
			}
			i += j + 1
		case c == '-' && strings.HasPrefix(query[i:], "--"):
			j := strings.IndexByte(query[i:], '\n')
			if j < 0 {
				i = len(query)
			} else {
				i += j
			}
		case c == '/' && strings.HasPrefix(query[i:], "/*"):
			j := strings.Index(query[i+2:], "*/")
			if j < 0 {
				i = len(query)
			} else {
				i += j + 3
			}
		case c == ';':
			end = i
		}
	}
	if !seen {
		return fmt.Errorf("%w: the query is empty", ErrBadQuery)
	}
	return nil
}

// startsWithKeyword reports whether s starts with one of the keywords, as a whole word, in any case.
func startsWithKeyword(s string, keywords ...string) bool {
	for _, k := range keywords {
		if len(s) >= len(k) && strings.EqualFold(s[:len(k)], k) && (len(s) == len(k) || !isWordByte(s[len(k)])) {
			return true
		}
	}
	return false
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}
