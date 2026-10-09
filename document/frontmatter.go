package document

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// bom is the UTF-8 byte order mark some editors put at the start of a file.
var bom = []byte("\ufeff")

// splitFrontmatter finds the frontmatter block: the file's first line is exactly "---", and the block
// ends at the next line that is "---" or "...". It returns nil and offset 0 when there is no block.
func splitFrontmatter(raw []byte) (*frontmatter, int, error) {
	start := 0
	if bytes.HasPrefix(raw, bom) {
		start = len(bom)
	}
	first, next := line(raw, start)
	if trimEOL(first) != "---" {
		return nil, 0, nil
	}
	eol := "\n"
	if strings.HasSuffix(first, "\r\n") {
		eol = "\r\n"
	}

	for pos := next; pos < len(raw); {
		l, after := line(raw, pos)
		if t := trimEOL(l); t == "---" || t == "..." {
			content := raw[next:pos]
			if len(content) > MaxFrontmatterSize {
				return nil, 0, fmt.Errorf("%w: frontmatter is larger than %d bytes", ErrUnusable, MaxFrontmatterSize)
			}
			f := &frontmatter{
				open:    string(raw[:next]),
				close:   l,
				content: content,
				eol:     eol,
			}
			f.decode()
			return f, after, nil
		}
		pos = after
	}
	return nil, 0, nil // never closed: not frontmatter
}

// line returns the line starting at pos, including its line ending, and where the next line starts.
func line(raw []byte, pos int) (string, int) {
	i := bytes.IndexByte(raw[pos:], '\n')
	if i < 0 {
		return string(raw[pos:]), len(raw)
	}
	return string(raw[pos : pos+i+1]), pos + i + 1
}

func trimEOL(s string) string {
	return strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
}

// decode parses the block into a YAML node tree, or records why it can't be used.
func (f *frontmatter) decode() {
	var doc yaml.Node
	err := yaml.Unmarshal(f.content, &doc)
	if err != nil {
		f.broken = "not valid YAML: " + strings.TrimPrefix(err.Error(), "yaml: ")
		return
	}
	if doc.Kind == 0 { // empty, or only comments
		f.node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		f.doc = &yaml.Node{
			Kind:        yaml.DocumentNode,
			HeadComment: strings.TrimSpace(string(f.content)),
			Content:     []*yaml.Node{f.node},
		}
		return
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		f.broken = "not a list of properties"
		return
	}
	if usesAnchors(root) {
		f.broken = "YAML anchors and aliases aren't allowed"
		return
	}
	seen := make(map[string]bool)
	for i := 0; i < len(root.Content); i += 2 {
		key := root.Content[i]
		if key.Kind != yaml.ScalarNode {
			f.broken = "property names must be plain text"
			return
		}
		k := key.Value
		if seen[k] {
			f.broken = fmt.Sprintf("the key %q appears more than once", k)
			return
		}
		seen[k] = true
	}
	f.doc = &doc
	f.node = root
}

func usesAnchors(n *yaml.Node) bool {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return true
	}
	return slices.ContainsFunc(n.Content, usesAnchors)
}

// The frontmatter keys speki owns.
const (
	keyTitle       = "title"
	keyDescription = "description"
	keyTags        = "tags"
	keyAliases     = "aliases"
	keyStatus      = "status"
	keyRelated     = "related"
	keyCreated     = "created"
	keyUpdated     = "updated"
)

// readMeta fills m from the block and returns the problems it ignored.
func (f *frontmatter) readMeta(m *Meta) []string {
	if f.broken != "" {
		return []string{"frontmatter: " + f.broken}
	}
	var warnings []string
	warn := func(format string, args ...any) {
		w := fmt.Sprintf(format, args...)
		if !slices.Contains(warnings, w) {
			warnings = append(warnings, w)
		}
	}

	pairs := f.node.Content
	for i := 0; i+1 < len(pairs); i += 2 {
		key, value := pairs[i].Value, pairs[i+1]
		switch key {
		case keyTitle:
			m.Title = scalarField(key, value, warn)
		case keyDescription:
			m.Description = scalarField(key, value, warn)
		case keyTags:
			m.Tags = normaliseTags(listField(key, value, warn))
		case keyAliases:
			m.Aliases = listField(key, value, warn)
		case keyRelated:
			m.Related = listField(key, value, warn)
		case keyStatus:
			s := scalarField(key, value, warn)
			switch Status(s) {
			case "":
			case StatusDraft, StatusActive, StatusDeprecated:
				m.Status = Status(s)
			default:
				warn("status: want draft, active or deprecated, got %q", s)
			}
		case keyCreated:
			m.Created = timeField(key, value, warn)
		case keyUpdated:
			m.Updated = timeField(key, value, warn)
		default:
			var v any
			err := value.Decode(&v)
			if err != nil {
				warn("%s: %v", key, err)
				continue
			}
			v, ok := jsonSafe(v)
			if !ok {
				warn("%s: nested property names must be text", key)
				continue
			}
			if m.Extra == nil {
				m.Extra = make(map[string]any)
			}
			m.Extra[key] = v
		}
	}
	return warnings
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

func scalarField(key string, n *yaml.Node, warn func(string, ...any)) string {
	if isNull(n) {
		return ""
	}
	if n.Kind != yaml.ScalarNode {
		warn("%s: want text", key)
		return ""
	}
	return strings.TrimSpace(n.Value)
}

// listField reads a list of text. A single value is accepted, split on commas, with a warning: Obsidian
// allows it, but speki writes lists.
func listField(key string, n *yaml.Node, warn func(string, ...any)) []string {
	switch {
	case isNull(n):
		return nil
	case n.Kind == yaml.ScalarNode:
		warn("%s: a single value instead of a list", key)
		var out []string
		for p := range strings.SplitSeq(n.Value, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	case n.Kind == yaml.SequenceNode:
		var out []string
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode || isNull(item) {
				warn("%s: want a list of text", key)
				continue
			}
			if v := strings.TrimSpace(item.Value); v != "" {
				out = append(out, v)
			}
		}
		return out
	default:
		warn("%s: want a list of text", key)
		return nil
	}
}

// normaliseTags drops a leading #, lowercases and removes duplicates, keeping the first occurrence.
func normaliseTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		t = strings.ToLower(strings.TrimPrefix(t, "#"))
		if t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// timeLayouts are the date formats speki reads: RFC 3339, and the forms Obsidian writes (taken as UTC).
var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func timeField(key string, n *yaml.Node, warn func(string, ...any)) time.Time {
	s := scalarField(key, n, warn)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range timeLayouts {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t.UTC()
		}
	}
	warn("%s: want a date and time, got %q", key, s)
	return time.Time{}
}

// jsonSafe converts decoded YAML into values encoding/json accepts. YAML allows maps with non-text keys,
// which JSON doesn't; those are refused.
func jsonSafe(v any) (any, bool) {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			safe, ok := jsonSafe(x)
			if !ok {
				return nil, false
			}
			v[k] = safe
		}
		return v, true
	case map[any]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			ks, ok := k.(string)
			if !ok {
				return nil, false
			}
			safe, ok := jsonSafe(x)
			if !ok {
				return nil, false
			}
			out[ks] = safe
		}
		return out, true
	case []any:
		for i, x := range v {
			safe, ok := jsonSafe(x)
			if !ok {
				return nil, false
			}
			v[i] = safe
		}
		return v, true
	default:
		return v, true
	}
}
