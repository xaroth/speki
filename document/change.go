package document

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Check compares a document an agent wants to write with the current one (nil for a new document) and
// returns the problems the change brings in. Problems the current document already had are left alone,
// so an agent fixing a typo isn't blocked by a person's messy frontmatter. Frontmatter keys other than
// the writer fields and allowed must keep the value they have in current.
func Check(current, next *Document, allowed []string) error {
	var problems []error
	if len(next.raw) > MaxSize {
		problems = append(problems, fmt.Errorf("%w: the document is larger than %d bytes", ErrTooLarge, MaxSize))
	}
	if next.front != nil && next.front.broken != "" {
		// Even when the current document has the same problem: Touch must rewrite the block.
		problems = append(problems, fmt.Errorf("frontmatter: %s (speki rewrites the frontmatter to set updated, so it has to be valid)", next.front.broken))
	} else {
		for _, w := range next.Warnings {
			if current == nil || !slices.Contains(current.Warnings, w) {
				problems = append(problems, errors.New(w))
			}
		}
		if current != nil {
			// The warnings match when the current document is over a limit too, but it mustn't grow.
			problems = append(problems,
				grown(keyTags, len(current.Meta.Tags), len(next.Meta.Tags), MaxTags),
				grown(keyAliases, len(current.Meta.Aliases), len(next.Meta.Aliases), MaxAliases))
		}
	}
	problems = append(problems, lockedChanges(current, next, allowed)...)
	if current == nil && next.titleFromPath {
		problems = append(problems, errors.New("title: missing; add a title to the frontmatter or start with a # heading"))
	}
	return errors.Join(problems...)
}

// grown returns an error when a list over its limit got longer.
func grown(key string, current, next, limit int) error {
	if next > limit && next > current {
		return fmt.Errorf("%s: more than %d; a change can't add more", key, limit)
	}
	return nil
}

// writerKeys are the frontmatter keys agents may always set.
var writerKeys = []string{keyTitle, keyDescription, keyTags, keyAliases, keyStatus, keyRelated}

// WriterKeys returns the frontmatter keys agents may always set.
func WriterKeys() []string { return slices.Clone(writerKeys) }

// SetBySpeki reports whether speki sets key itself.
func SetBySpeki(key string) bool { return key == keyCreated || key == keyUpdated }

// lockedChanges returns an error for each frontmatter key outside the writer fields and allowed that next
// adds, removes or changes. The text doesn't vary with the value, like the warnings'.
func lockedChanges(current, next *Document, allowed []string) []error {
	free := func(key string) bool {
		return slices.Contains(writerKeys, key) || slices.Contains(allowed, key) || key == keyUpdated // updated is always overwritten
	}
	createdSet := current == nil || !current.Meta.Created.IsZero() // Touch sets it to now, or to the current value
	if current != nil && current.front != nil && current.front.broken != "" {
		// Its keys can't be compared, so a repair may only drop keys agents can set anyway.
		keys, ok := topLevelKeys(current.front.content)
		if !ok {
			return []error{errors.New("frontmatter: the current frontmatter is broken and speki can't tell which keys it holds; a person has to fix it")}
		}
		var problems []error
		for _, k := range keys {
			if !free(k) && k != keyCreated {
				problems = append(problems, fmt.Errorf("frontmatter: the current frontmatter is broken and holds %q, which agents can't set; a person has to fix it", k))
			}
		}
		if problems != nil {
			return problems
		}
		createdSet = createdSet || slices.Contains(keys, keyCreated)
	}

	cur, nxt := properties(current), properties(next)
	var problems []error
	check := func(key string) {
		if free(key) || key == keyCreated && createdSet {
			return
		}
		a, inCur := cur[key]
		b, inNext := nxt[key]
		var what string
		switch {
		case !inCur && inNext:
			what = "added"
		case inCur && !inNext:
			what = "removed"
		case inCur && !sameValue(a, b):
			what = "changed"
		default:
			return
		}
		problems = append(problems, fmt.Errorf("frontmatter: %q can't be %s; agents may only set %s in this vault, other keys must stay as they are",
			key, what, strings.Join(append(slices.Clone(writerKeys), allowed...), ", ")))
	}
	for _, key := range keysOf(next) {
		check(key)
	}
	for _, key := range keysOf(current) {
		if _, ok := nxt[key]; !ok {
			check(key)
		}
	}
	return problems
}

// topLevelKey matches a line holding a top-level key: plain, or in single or double quotes.
var topLevelKey = regexp.MustCompile(`^(?:"([^"]*)"|'([^']*)'|([^\s#'"\-\[{][^:#]*?))\s*:(?:\s|$)`)

// topLevelKeys reads the top-level key names from a frontmatter block that isn't valid YAML, line by line.
// It returns false for anything it can't be sure about, since a missed key could be dropped unseen: other
// line breaks than YAML's usual, tabs or an indented root, and indented lines under a key whose value is
// on its own line.
func topLevelKeys(content []byte) ([]string, bool) {
	s := strings.ReplaceAll(string(content), "\r\n", "\n")
	if strings.ContainsAny(s, "\r\u0085\u2028\u2029") {
		return nil, false
	}
	var keys []string
	nested := false // whether indented lines and list items can belong to the last key
	for l := range strings.SplitSeq(s, "\n") {
		switch {
		case strings.TrimLeft(l, " ") == "", strings.HasPrefix(strings.TrimLeft(l, " "), "#"):
			continue
		case l[0] == ' ' || l == "-" || strings.HasPrefix(l, "- "):
			if !nested {
				return nil, false
			}
			continue
		}
		m := topLevelKey.FindStringSubmatch(l)
		if m == nil {
			return nil, false
		}
		keys = append(keys, m[1]+m[2]+strings.TrimSpace(m[3]))
		v := strings.TrimSpace(l[len(m[0]):])
		nested = v == "" || v[0] == '|' || v[0] == '>' || v[0] == '#'
	}
	return keys, true
}

// keysOf and properties read keys as text: decode refuses blocks with other keys or duplicates.
func keysOf(d *Document) []string {
	if d == nil || d.front == nil || d.front.node == nil {
		return nil
	}
	var keys []string
	pairs := d.front.node.Content
	for i := 0; i+1 < len(pairs); i += 2 {
		keys = append(keys, pairs[i].Value)
	}
	return keys
}

// properties maps the frontmatter's keys to their values. It's empty without usable frontmatter.
func properties(d *Document) map[string]*yaml.Node {
	out := make(map[string]*yaml.Node)
	if d == nil || d.front == nil || d.front.node == nil {
		return out
	}
	pairs := d.front.node.Content
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i].Value] = pairs[i+1]
	}
	return out
}

// sameValue compares two YAML values, ignoring style, comments and position.
func sameValue(a, b *yaml.Node) bool {
	if a.Kind != b.Kind || a.ShortTag() != b.ShortTag() || len(a.Content) != len(b.Content) {
		return false
	}
	if a.Kind == yaml.ScalarNode && a.ShortTag() != "!!null" && a.Value != b.Value { // null, ~ and empty are one value
		return false
	}
	for i := range a.Content {
		if !sameValue(a.Content[i], b.Content[i]) {
			return false
		}
	}
	return true
}

// Touch sets updated to now in the frontmatter, and created to created unless that's zero. It returns the
// new file and leaves d unchanged. Only the frontmatter block is rewritten: the body is kept byte for
// byte, and the frontmatter keeps its line endings.
func (d *Document) Touch(now, created time.Time) ([]byte, error) {
	f := d.front
	body := []byte(d.Body)
	if f == nil {
		eol := eolOf(d.raw)
		open := "---" + eol
		if bytes.HasPrefix(d.raw, bom) { // keep a byte order mark at the very start
			open = string(bom) + open
		}
		node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		f = &frontmatter{
			open:  open,
			close: "---" + eol,
			eol:   eol,
			doc:   &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{node}},
			node:  node,
		}
	}
	if f.broken != "" {
		return nil, fmt.Errorf("frontmatter: %s", f.broken)
	}

	doc := copyNode(f.doc) // d stays as parsed
	node := doc.Content[0]
	if !created.IsZero() {
		setKey(node, keyCreated, created.UTC().Format(time.RFC3339))
	}
	setKey(node, keyUpdated, now.UTC().Format(time.RFC3339))

	encoded, err := encodeStable(doc)
	if err != nil {
		return nil, err
	}
	if f.eol == "\r\n" {
		encoded = strings.ReplaceAll(encoded, "\n", "\r\n")
	}

	closing := f.close
	if !strings.HasSuffix(closing, "\n") { // the block closed on the file's last line
		closing = trimEOL(closing) + f.eol
	}

	if len(encoded) > maxFrontmatterSize {
		return nil, fmt.Errorf("%w: the frontmatter would be larger than %d bytes once speki sets the dates", ErrTooLarge, maxFrontmatterSize)
	}
	var out bytes.Buffer
	out.WriteString(f.open)
	out.WriteString(encoded)
	out.WriteString(closing)
	out.Write(body)
	if out.Len() > MaxSize {
		return nil, fmt.Errorf("%w: the document would be larger than %d bytes once speki sets the dates", ErrTooLarge, MaxSize)
	}
	return out.Bytes(), nil
}

// encodeStable encodes doc so that decoding and encoding the result gives the same text. The YAML
// encoder sometimes moves a comment or adds a blank line that the decoder reads back differently, which
// would make every Touch change the block again. A few rounds settle it.
func encodeStable(doc *yaml.Node) (string, error) {
	out, err := encode(doc)
	for range 3 {
		if err != nil {
			return "", err
		}
		var again yaml.Node
		err = yaml.Unmarshal([]byte(out), &again)
		if err != nil {
			return "", fmt.Errorf("encode frontmatter: %w", err)
		}
		var next string
		next, err = encode(&again)
		if err == nil && next == out {
			return out, nil
		}
		out = next
	}
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("%w; move or remove them", ErrUnstable)
}

func encode(doc *yaml.Node) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	err := enc.Encode(doc)
	if err != nil {
		return "", fmt.Errorf("encode frontmatter: %w", err)
	}
	err = enc.Close()
	if err != nil {
		return "", fmt.Errorf("encode frontmatter: %w", err)
	}
	return buf.String(), nil
}

func copyNode(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		c.Content[i] = copyNode(child)
	}
	return &c
}

// eolOf returns the line ending the file uses, "\n" when it has none.
func eolOf(raw []byte) string {
	i := bytes.IndexByte(raw, '\n')
	if i > 0 && raw[i-1] == '\r' {
		return "\r\n"
	}
	return "\n"
}

// setKey sets key to a plain scalar value, replacing the existing value node or appending a new pair.
func setKey(m *yaml.Node, key, value string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			comment := v.LineComment
			*v = yaml.Node{
				Kind:        yaml.ScalarNode,
				Tag:         "!!timestamp",
				Value:       value,
				LineComment: comment,
			}
			return
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	if n := len(m.Content); n >= 2 {
		// A comment below the last property stays below the properties.
		last := m.Content[n-2]
		k.FootComment, last.FootComment = last.FootComment, ""
	}
	m.Content = append(m.Content, k, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!timestamp", Value: value})
}
