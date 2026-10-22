package apps

import (
	"slices"
	"testing"
)

func TestParseExec(t *testing.T) {
	cases := map[string][]string{
		`firefox %u`: {"firefox"},
		`env BAMF_DESKTOP_FILE_HINT=/var/lib/snapd/desktop/applications/firefox_firefox.desktop /snap/bin/firefox %u`: {
			"env", "BAMF_DESKTOP_FILE_HINT=/var/lib/snapd/desktop/applications/firefox_firefox.desktop", "/snap/bin/firefox"},
		`"/opt/My App/app" --name "a b" 100%%`: {"/opt/My App/app", "--name", "a b", "100%"},
		`sh -c "echo \"hi\""`:                  {"sh", "-c", `echo "hi"`},
	}
	for in, want := range cases {
		if got := ParseExec(in); !slices.Equal(got, want) {
			t.Errorf("ParseExec(%q) = %q, want %q", in, got, want)
		}
	}
	if got := execName(`env A=1 /snap/bin/firefox %u`); got != "firefox" {
		t.Errorf("execName = %q", got)
	}
}
