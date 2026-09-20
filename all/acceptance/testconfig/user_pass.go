package testconfig

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const maxUserPassConfigBytes = 128 * 1024

// UserPassConfig is the explicitly selected, credentials-only physical-test
// schema. It is not an alternative acceptance vault or a --ready fixture.
type UserPassConfig struct {
	user string
	pass string
}

func LoadUserPass(path string) (*UserPassConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("read user-pass config failed")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUserPassConfigBytes+1))
	if err != nil || len(data) > maxUserPassConfigBytes {
		return nil, errors.New("user-pass config exceeds bounded input or cannot be read")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		// YAML diagnostics can contain scalar values. Never propagate them.
		return nil, errors.New("invalid user-pass config")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("user-pass config must contain one document")
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return nil, errors.New("user-pass config must contain user and pass strings")
	}
	mapping := document.Content[0]
	if mapping.Kind != yaml.MappingNode || mapping.Tag != "!!map" || len(mapping.Content) != 4 {
		return nil, errors.New("user-pass config must contain only user and pass strings")
	}
	config := &UserPassConfig{}
	seen := map[string]bool{}
	for i := 0; i < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" ||
			(key.Value != "user" && key.Value != "pass") || seen[key.Value] ||
			value.Kind != yaml.ScalarNode || value.Tag != "!!str" || strings.TrimSpace(value.Value) == "" {
			return nil, errors.New("user-pass config requires distinct nonblank user and pass strings")
		}
		seen[key.Value] = true
		if key.Value == "user" {
			config.user = value.Value
		} else {
			config.pass = value.Value
		}
	}
	return config, nil
}

func (c *UserPassConfig) Get(key string) (string, error) {
	switch key {
	case "user":
		return c.user, nil
	case "pass":
		return c.pass, nil
	default:
		return "", errors.New("user-pass schema only supports user and pass keys")
	}
}
