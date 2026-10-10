package document

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Check compares a document an agent wants to write with the current one (nil for a new document) and
// returns the problems the change brings in. Problems the current document already had are left alone,
// so an agent fixing a typo isn't blocked by a person's messy frontmatter.
func Check(current, next *Document) error {
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
	}
	if current == nil && next.titleFromName {
		problems = append(problems, errors.New("title: missing; add a title to the frontmatter or start with a # heading"))
	}
	return errors.Join(problems...)
}

// Touch sets updated to now in the frontmatter, and created too when created is true and the document
// has no valid created date. It returns the new file and leaves d unchanged. Only the frontmatter block is rewritten: the
// body is kept byte for byte, and the frontmatter keeps its line endings.
func (d *Document) Touch(now time.Time, created bool) ([]byte, error) {
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
	stamp := now.UTC().Format(time.RFC3339)
	if created && d.Meta.Created.IsZero() {
		setKey(node, keyCreated, stamp)
	}
	setKey(node, keyUpdated, stamp)

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

	if len(encoded) > MaxFrontmatterSize {
		return nil, fmt.Errorf("%w: the frontmatter would be larger than %d bytes once speki sets the dates", ErrTooLarge, MaxFrontmatterSize)
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
			*v = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!timestamp", Value: value, LineComment: comment}
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
