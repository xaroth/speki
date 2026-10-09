package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
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

// decodeYAML decodes raw into cfg, rejecting unknown keys: a typo should fail, not be ignored.
func decodeYAML(raw []byte, cfg *service.Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	err := dec.Decode(cfg)
	if errors.Is(err, io.EOF) {
		return nil // empty file
	}
	return err
}

// applyEnv sets every field of cfg that has a matching environment variable. The variable name is the
// field's YAML path in upper case, joined with underscores: telemetry.log_level is
// SPEKI_TELEMETRY_LOG_LEVEL. Other SPEKI_* variables are ignored; orchestrators add their own (a
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
	if v.Kind() == reflect.String {
		v.SetString(s)
		return nil
	}
	return fmt.Errorf("unsupported type %s", v.Type())
}
