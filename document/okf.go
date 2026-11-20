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

// The OKF files with a fixed meaning. They aren't concepts: they need no type or title, and speki doesn't
// date them.
const (
	okfIndex = "index.md"
	okfLog   = "log.md"
)

func okfReserved(docPath string) bool {
	b := path.Base(docPath)
	return b == okfIndex || b == okfLog
}

// IsOKFReserved reports whether docPath names an index.md or log.md, which OKF gives a fixed meaning.
func IsOKFReserved(docPath string) bool { return okfReserved(docPath) }

// CheckOKF is Check for a vault in the Open Knowledge Format. Agents may also set OKF's keys, and
// generated and timestamp, which speki overwrites; created stays as it is. A concept needs a type, an
// index.md has no frontmatter except okf_version in the vault's root one, and only login may be added as
// a human verifier. Like Check, it leaves alone problems the current document (nil for a new one) already
// had.
func CheckOKF(current, next *Document, allowed []string, login string) error {
	allowed = slices.Concat(okfKeys, []string{keyGenerated, keyTimestamp}, allowed)
	if next.path == okfIndex {
		allowed = append(allowed, keyOKFVersion)
	}
	return errors.Join(check(current, next, allowed, true), okfRules(current, next), humanVerifiers(current, next, login))
}

func okfRules(current, next *Document) error {
	switch path.Base(next.path) {
	case okfLog:
		return nil
	case okfIndex:
		if next.path == okfIndex {
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

// humanVerifiers refuses verified entries by a person other than login that current doesn't have: an
// agent can't claim someone else reviewed a document.
func humanVerifiers(current, next *Document, login string) error {
	var cur []string
	if current != nil {
		cur = verifiers(current)
	}
	var problems []error
	for _, v := range verifiers(next) {
		by, _, _ := strings.Cut(v, "\x00")
		person, ok := strings.CutPrefix(by, "human:")
		if !ok || strings.EqualFold(person, login) || slices.Contains(cur, v) {
			continue
		}
		problems = append(problems, fmt.Errorf("verified: %q can't be added; only the signed-in user, human:%s, can be added as a person who verified it", by, login))
	}
	return errors.Join(problems...)
}

// verifiers returns the verified events as "by\x00at". A single mapping counts as a list of one.
func verifiers(d *Document) []string {
	var events []any
	switch v := d.Meta.Extra["verified"].(type) {
	case map[string]any:
		events = []any{v}
	case []any:
		events = v
	}
	var out []string
	for _, e := range events {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, fmt.Sprint(m["by"])+"\x00"+fmt.Sprint(m["at"]))
	}
	return out
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
	if okfReserved(d.path) {
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
