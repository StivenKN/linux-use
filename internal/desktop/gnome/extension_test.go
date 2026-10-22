package gnome

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

// metadata.json must declare the running shell, however new, and the
// embedded script must report the version the backend expects.
func TestExtensionFiles(t *testing.T) {
	for shell, want := range map[int][]string{
		47: {"45", "46", "47", "48", "49", "50"},
		52: {"45", "46", "47", "48", "49", "50", "51", "52"},
	} {
		var meta struct {
			UUID         string   `json:"uuid"`
			ShellVersion []string `json:"shell-version"`
		}
		files := extensionFiles(shell)
		if err := json.Unmarshal(files["metadata.json"], &meta); err != nil {
			t.Fatal(err)
		}
		if meta.UUID != ExtensionUUID || !slices.Equal(meta.ShellVersion, want) {
			t.Errorf("GNOME %d: metadata %+v, want shell-version %q", shell, meta, want)
		}
	}
	if !bytes.Contains(extensionJS, fmt.Appendf(nil, "const VERSION = %d;", extVersion)) {
		t.Errorf("extension.js does not declare VERSION = %d", extVersion)
	}
}

func TestStrv(t *testing.T) {
	for in, want := range map[string][]string{
		"@as []":                         nil,
		"[]":                             nil,
		"['a@b.io']":                     {"a@b.io"},
		`['a@b.io', "it's@x", 'q\'s@y']`: {"a@b.io", "it's@x", "q's@y"},
	} {
		got, err := parseStrv(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("parseStrv(%q) = %q, %v; want %q", in, got, err, want)
		}
		if back, err := parseStrv(formatStrv(want)); err != nil || !slices.Equal(back, want) {
			t.Errorf("parseStrv(formatStrv(%q)) = %q, %v", want, back, err)
		}
	}
	if _, err := parseStrv("['unterminated]"); err == nil {
		t.Error("parseStrv accepted an unterminated string")
	}
}
