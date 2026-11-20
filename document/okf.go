package document

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Format is the convention a vault's documents follow.
type Format string

// The formats a vault can use. Speki's own is the default.
const (
	FormatSpeki Format = "speki"
	FormatOKF   Format = "okf"
)

// The OKF keys speki reads or sets. https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md
const (
	keyType       = "type"
	keyGenerated  = "generated"
	keyTimestamp  = "timestamp"
	keyOKFVersion = "okf_version"
)

// okfKeys are the frontmatter keys agents may set in an OKF vault on top of the writer fields.
var okfKeys = []string{keyType, "resource", "sources", "usage_window", "verified", "stale_after"}

// OKFKeys returns the frontmatter keys agents may set in an OKF vault on top of the writer fields.
func OKFKeys() []string { return slices.Clone(okfKeys) }

// OKFAllow returns what Check needs as allowed in an OKF vault whose configuration allows allow: OKF's
// keys, and generated and timestamp, which speki overwrites like updated.
func OKFAllow(allow []string) []string {
	return slices.Concat(okfKeys, []string{keyGenerated, keyTimestamp}, allow)
}

// The OKF files with a fixed meaning. They aren't concepts: they need no type and speki doesn't date them.
const (
	okfIndex = "index.md"
	okfLog   = "log.md"
)

// CheckOKF returns the problems a change brings in under OKF's rules: a concept needs a type, and an
// index.md has no frontmatter, except okf_version in the vault's root one. Like Check, it leaves alone
// problems the current document (nil for a new one) already had.
func CheckOKF(current, next *Document) error {
	switch path.Base(next.path) {
	case okfLog:
		return nil
	case okfIndex:
		if !strings.Contains(next.path, "/") {
			var problems []error
			for _, k := range keysOf(next) {
				if k != keyOKFVersion && !slices.Contains(keysOf(current), k) {
					problems = append(problems, fmt.Errorf("frontmatter: %q can't be added; the root index.md of an OKF vault holds only %s", k, keyOKFVersion))
				}
			}
			return errors.Join(problems...)
		}
		if next.front != nil && (current == nil || current.front == nil) {
			return errors.New("frontmatter: an index.md in an OKF vault has no frontmatter; remove the block")
		}
		return nil
	}
	if !hasType(next) && (current == nil || hasType(current)) {
		return errors.New("type: missing; OKF documents need a type that says what kind of thing they describe, such as Reference, Guide or Playbook")
	}
	return nil
}

func hasType(d *Document) bool {
	t, ok := d.Meta.Extra[keyType].(string)
	return ok && strings.TrimSpace(t) != ""
}

// TouchOKF sets the last change in an OKF document to now, made by by, and returns the new file. A
// document that records it in generated (OKF v0.2) or timestamp (v0.1) keeps that key; one that has
// neither gets generated, or timestamp when v01 is set. updated is set too when the document has it.
// index.md and log.md are returned unchanged.
func (d *Document) TouchOKF(now time.Time, by string, v01 bool) ([]byte, error) {
	if b := path.Base(d.path); b == okfIndex || b == okfLog {
		return bytes.Clone(d.raw), nil
	}
	at := now.UTC().Format(time.RFC3339)
	return d.rewrite(func(node *yaml.Node) {
		gen, ts := valueOf(node, keyGenerated), valueOf(node, keyTimestamp)
		if gen != nil || ts == nil && !v01 {
			setGenerated(node, gen, by, at)
		}
		if ts != nil || gen == nil && v01 {
			setKey(node, keyTimestamp, at)
		}
		if valueOf(node, keyUpdated) != nil {
			setKey(node, keyUpdated, at)
		}
	})
}

// setGenerated sets generated.by and generated.at, keeping other keys in it. A generated that isn't a
// mapping is replaced.
func setGenerated(node, gen *yaml.Node, by, at string) {
	if gen == nil || gen.Kind != yaml.MappingNode {
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setScalar(m, "by", by, "!!str")
		setScalar(m, "at", at, "!!timestamp")
		if gen == nil {
			appendKey(node, keyGenerated, m)
			return
		}
		m.LineComment = gen.LineComment
		*gen = *m
		return
	}
	setScalar(gen, "by", by, "!!str")
	setScalar(gen, "at", at, "!!timestamp")
}

// valueOf returns the value of key in the mapping m, or nil.
func valueOf(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
