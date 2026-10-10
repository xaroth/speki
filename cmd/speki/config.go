package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
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

// loadConfig reads the YAML file at path, then applies SPEKI_* environment variables on top. An empty path
// means the default file, which may be missing, so speki can run on environment variables alone. A file
// named explicitly must exist.
func loadConfig(path string, environ []string) (service.Config, error) {
	var cfg service.Config

	explicit := path != ""
	if !explicit {
		path = defaultConfigPath
	}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		// Nothing to read: everything comes from the environment.
	case err != nil:
		return cfg, fmt.Errorf("read config: %w", err)
	default:
		err = decodeYAML(raw, &cfg)
		if err != nil {
			return cfg, fmt.Errorf("config %s: %w", path, err)
		}
	}

	err = applyEnv(&cfg, environ)
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

// decodeYAML decodes raw into cfg, rejecting unknown keys: a typo should fail, not be ignored. A secret
// can also be given as a file: private_key_file instead of private_key.
func decodeYAML(raw []byte, cfg *service.Config) error {
	var doc yaml.Node
	err := yaml.Unmarshal(raw, &doc)
	if err != nil {
		return err
	}
	if doc.Kind == 0 {
		return nil // empty file
	}
	err = readSecretFiles(doc.Content[0], reflect.TypeFor[service.Config]())
	if err != nil {
		return err
	}
	resolved, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(resolved))
	dec.KnownFields(true)
	return dec.Decode(cfg)
}

// readSecretFiles replaces every "<secret>_file: path" in a mapping node by "<secret>: content", for the
// fields of t that are secrets, and recurses into nested sections.
func readSecretFiles(n *yaml.Node, t reflect.Type) error {
	if n.Kind != yaml.MappingNode || t.Kind() != reflect.Struct {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if f, ok := fieldByTag(t, key.Value); ok {
			err := readSecretFiles(value, f.Type)
			if err != nil {
				return err
			}
			continue
		}
		base, ok := strings.CutSuffix(key.Value, "_file")
		if !ok {
			continue // unknown: the strict decode reports it
		}
		f, ok := fieldByTag(t, base)
		if !ok || f.Type != reflect.TypeFor[service.Secret]() {
			continue
		}
		if hasKey(n, base) {
			return fmt.Errorf("%s and %s are both set", base, key.Value)
		}
		content, err := readSecret(value.Value)
		if err != nil {
			return fmt.Errorf("%s: %w", key.Value, err)
		}
		*key = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: base}
		*value = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: content}
	}
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
		if name == tag {
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
// SPEKI_TELEMETRY_LOG_LEVEL. A secret can also come from a file named by the variable with _FILE added.
// Lists are comma separated. Other SPEKI_* variables are ignored; orchestrators add their own (a
// Kubernetes service called speki brings SPEKI_PORT).
func applyEnv(cfg *service.Config, environ []string) error {
	env := make(map[string]string)
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, envPrefix) {
			env[k] = v
		}
	}
	return walkEnv(reflect.ValueOf(cfg).Elem(), strings.TrimSuffix(envPrefix, "_"), env)
}

func walkEnv(v reflect.Value, prefix string, env map[string]string) error {
	t := v.Type()
	for i := range t.NumField() {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		name := prefix + "_" + strings.ToUpper(tag)
		fv := v.Field(i)

		if fv.Kind() == reflect.Struct {
			err := walkEnv(fv, name, env)
			if err != nil {
				return err
			}
			continue
		}

		s, ok := env[name]
		if file, isFile := env[name+"_FILE"]; isFile && fv.Type() == reflect.TypeFor[service.Secret]() {
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
