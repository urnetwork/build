package testconfig

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUserPassExplicitSchemaPreservesRawScalars(t *testing.T) {
	values := map[string]string{"user": "physical@example.invalid", "pass": "  quotes ' \" $() \\ unicode-雪  "}
	data, err := yaml.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, string(data), 0o600)
	config, err := LoadUserPass(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range values {
		if got, err := config.Get(key); err != nil || got != want {
			t.Fatal("selected scalar was not preserved")
		}
	}
	for _, key := range []string{"email", "password", "data_plane_account.email", "data_plane_account.password"} {
		if _, err := config.Get(key); err == nil {
			t.Fatal("unexpected schema fallback")
		}
	}
	if _, err := Load(path); err == nil {
		t.Fatal("default acceptance loader accepted the user-pass schema")
	}
}

func TestUserPassRejectsWrongSchemaAndUnsafeShapesWithoutValues(t *testing.T) {
	for name, content := range map[string]string{
		"vault-schema":       "version: 1\ndata_plane_account: {email: private-marker, password: private-marker}\n",
		"missing":            "user: private-marker\n",
		"extra":              "user: private-marker\npass: private-marker\nextra: private-marker\n",
		"duplicate":          "user: private-marker\nuser: private-marker\n",
		"non-string":         "user: private-marker\npass: 123456\n",
		"null":               "user: private-marker\npass: null\n",
		"blank":              "user: private-marker\npass: '   '\n",
		"sequence":           "user: private-marker\npass: [private-marker]\n",
		"alias":              "user: &value private-marker\npass: *value\n",
		"malformed":          "user: private-marker\npass: [private-marker\n",
		"multiple-documents": "user: private-marker\npass: private-marker\n---\nuser: private-marker\npass: private-marker\n",
		"custom-tag":         "!custom {user: private-marker, pass: private-marker}\n",
		"oversized":          "user: private-marker\npass: " + strings.Repeat("x", maxUserPassConfigBytes),
	} {
		t.Run(name, func(t *testing.T) {
			if config, err := LoadUserPass(writeConfig(t, content, 0o600)); err == nil || config != nil {
				t.Fatal("invalid schema accepted")
			} else if strings.Contains(err.Error(), "private-marker") {
				t.Fatal("parser error leaked a scalar")
			}
		})
	}
}
