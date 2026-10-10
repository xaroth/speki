package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/eve-online-tools/speki/service"
)

const (
	defaultConfigPath = "config.yaml"
	envPrefix         = "SPEKI_"
)

// loadConfig reads the YAML file at path, then applies SPEKI_* environment variables on top, then reads
// the secret files the YAML names for secrets the environment didn't set. An empty path means the default
// file, which may be missing, so speki can run on environment variables alone. A file named explicitly
// must exist.
func loadConfig(path string, environ []string) (service.Config, error) {
	var cfg service.Config

	explicit := path != ""
	if !explicit {
		path = defaultConfigPath
	}
	raw, err := os.ReadFile(path)
	var files []secretFile
	switch {
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		// Nothing to read: everything comes from the environment.
	case err != nil:
		return cfg, fmt.Errorf("read config: %w", err)
	default:
		files, err = decodeYAML(raw, &cfg)
		if err != nil {
			return cfg, fmt.Errorf("config %s: %w", path, err)
		}
	}

	set, err := applyEnv(&cfg, environ)
	if err != nil {
		return cfg, err
	}
	for _, f := range files {
		if set[f.env] {
			continue // the environment wins, so a file baked into an image can be overridden
		}
		content, err := readSecret(f.path)
		if err != nil {
			return cfg, fmt.Errorf("config %s: line %d: %s: %w", path, f.line, f.key, err)
		}
		reflect.ValueOf(&cfg).Elem().FieldByIndex(f.field).SetString(content)
	}
	return cfg, nil
}

// secretFile is a "<secret>_file: path" entry of the YAML, read once the environment has had its say.
type secretFile struct {
	key   string // as written, private_key_file
	line  int    // where it was written
	path  string // the file
	env   string // the variable that overrides it, SPEKI_GITHUB_PRIVATE_KEY
	field []int  // the secret it fills in, as an index path into service.Config
}

// decodeYAML decodes raw into cfg, rejecting unknown keys: a typo should fail, not be ignored. Any text
// setting, meant for secrets, can also be given as a file: private_key_file instead of private_key. Those
// entries are returned, not read. The YAML is decoded as written, so errors name the right lines.
func decodeYAML(raw []byte, cfg *service.Config) ([]secretFile, error) {
	var doc yaml.Node
	err := yaml.Unmarshal(raw, &doc)
	if err != nil {
		return nil, err
	}
	if doc.Kind == 0 || len(doc.Content) == 0 || doc.Content[0].Tag == "!!null" {
		return nil, nil // empty
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: want a mapping of settings", root.Line)
	}
	var files []secretFile
	err = takeSecretFiles(root, reflect.TypeFor[service.Config](), nil, strings.TrimSuffix(envPrefix, "_"), &files)
	if err != nil {
		return nil, err
	}
	return files, root.Decode(cfg)
}

// takeSecretFiles checks the keys of a mapping node against the fields of t, recursing into nested
// sections. It reports unknown and repeated keys, and takes every "<secret>_file: path" out of the node
// into files. Anchors, aliases and merge keys are refused rather than half checked: settings files
// don't need them.
func takeSecretFiles(n *yaml.Node, t reflect.Type, index []int, prefix string, files *[]secretFile) error {
	var kept []*yaml.Node
	seen := make(map[string]bool)
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if key.Value == "<<" || key.Anchor != "" || value.Anchor != "" || value.Kind == yaml.AliasNode {
			return fmt.Errorf("line %d: anchors, aliases and merge keys aren't supported in the config file", key.Line)
		}
		if seen[key.Value] {
			return fmt.Errorf("line %d: %s appears twice", key.Line, key.Value)
		}
		seen[key.Value] = true
		if f, ok := fieldByTag(t, key.Value); ok {
			if f.Type.Kind() == reflect.Struct && value.Kind == yaml.MappingNode {
				err := takeSecretFiles(value, f.Type, slices.Concat(index, f.Index), prefix+"_"+strings.ToUpper(key.Value), files)
				if err != nil {
					return err
				}
			}
			kept = append(kept, key, value)
			continue
		}
		base, isFile := strings.CutSuffix(key.Value, "_file")
		f, ok := fieldByTag(t, base)
		if !isFile || !ok || f.Type.Kind() != reflect.String {
			return fmt.Errorf("line %d: unknown setting %q", key.Line, key.Value)
		}
		if hasKey(n, base) {
			return fmt.Errorf("line %d: %s and %s are both set", key.Line, base, key.Value)
		}
		if value.Kind != yaml.ScalarNode || value.Value == "" {
			return fmt.Errorf("line %d: %s: want a file path", value.Line, key.Value)
		}
		*files = append(*files, secretFile{
			key:   key.Value,
			line:  key.Line,
			path:  value.Value,
			env:   prefix + "_" + strings.ToUpper(base),
			field: slices.Concat(index, f.Index),
		})
	}
	n.Content = kept
	return nil
}

func hasKey(n *yaml.Node, key string) bool {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return true
		}
	}
	return false
}

func fieldByTag(t reflect.Type, tag string) (reflect.StructField, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == tag && name != "" && name != "-" {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// readSecret reads a secret file. A trailing line break, which editors add, isn't part of the secret.
func readSecret(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

// applyEnv sets every field of cfg that has a matching environment variable. The variable name is the
// field's YAML path in upper case, joined with underscores: telemetry.log_level is
// SPEKI_TELEMETRY_LOG_LEVEL. A text setting, meant for secrets, can also come from a file named by the
// variable with _FILE added. Lists are comma separated. Other SPEKI_* variables are ignored; orchestrators
// add their own (a Kubernetes service called speki brings SPEKI_PORT).
// It returns the variables it used.
func applyEnv(cfg *service.Config, environ []string) (map[string]bool, error) {
	env := make(map[string]string)
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, envPrefix) {
			env[k] = v
		}
	}
	set := make(map[string]bool)
	return set, walkEnv(reflect.ValueOf(cfg).Elem(), strings.TrimSuffix(envPrefix, "_"), env, set)
}

func walkEnv(v reflect.Value, prefix string, env map[string]string, set map[string]bool) error {
	t := v.Type()
	for i := range t.NumField() {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		name := prefix + "_" + strings.ToUpper(tag)
		fv := v.Field(i)

		if fv.Kind() == reflect.Struct {
			err := walkEnv(fv, name, env, set)
			if err != nil {
				return err
			}
			continue
		}

		s, ok := env[name]
		if file, isFile := env[name+"_FILE"]; isFile && fv.Kind() == reflect.String {
			if ok {
				return fmt.Errorf("%s and %s_FILE are both set", name, name)
			}
			content, err := readSecret(file)
			if err != nil {
				return fmt.Errorf("%s_FILE: %w", name, err)
			}
			s, ok = content, true
		}
		if !ok {
			continue
		}
		err := setFromString(fv, s)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		set[name] = true
	}
	return nil
}

// setFromString parses s into v according to v's type.
func setFromString(v reflect.Value, s string) error {
	if v.Type() == reflect.TypeFor[time.Duration]() {
		d, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		v.SetInt(int64(d))
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Slice:
		var parts []string
		if strings.TrimSpace(s) != "" {
			parts = strings.Split(s, ",")
		}
		list := reflect.MakeSlice(v.Type(), len(parts), len(parts))
		for i, p := range parts {
			err := setFromString(list.Index(i), strings.TrimSpace(p))
			if err != nil {
				return err
			}
		}
		v.Set(list)
	default:
		return fmt.Errorf("unsupported type %s", v.Type())
	}
	return nil
}
