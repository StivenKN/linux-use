// Package apps finds installed applications through XDG desktop entries and
// launches them detached into a graphical session.
package apps

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// App is one launchable desktop entry.
type App struct {
	ID       string `json:"id"` // desktop file id, e.g. "org.gnome.Nautilus"
	Name     string `json:"name"`
	Generic  string `json:"generic,omitempty"`
	Exec     string `json:"-"`
	Keywords string `json:"-"`
}

// dataDirs returns XDG data dirs in precedence order, plus snap and flatpak
// export dirs that sessions started outside a login shell often miss.
func dataDirs() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{os.Getenv("XDG_DATA_HOME")}
	if dirs[0] == "" {
		dirs[0] = filepath.Join(home, ".local/share")
	}
	sys := os.Getenv("XDG_DATA_DIRS")
	if sys == "" {
		sys = "/usr/local/share:/usr/share"
	}
	dirs = append(dirs, filepath.SplitList(sys)...)
	dirs = append(dirs,
		filepath.Join(home, ".local/share/flatpak/exports/share"),
		"/var/lib/flatpak/exports/share",
		"/var/lib/snapd/desktop",
	)
	return slices.Compact(dirs)
}

// List returns every visible application, first match per id winning.
func List() []App {
	seen := map[string]bool{}
	var out []App
	for _, d := range dataDirs() {
		root := filepath.Join(d, "applications")
		filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(path, ".desktop") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			id := strings.TrimSuffix(strings.ReplaceAll(rel, "/", "-"), ".desktop")
			if seen[id] {
				return nil
			}
			seen[id] = true
			if app, ok := parse(path, id); ok {
				out = append(out, app)
			}
			return nil
		})
	}
	slices.SortFunc(out, func(a, b App) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}

func parse(path, id string) (App, bool) {
	f, err := os.Open(path)
	if err != nil {
		return App{}, false
	}
	defer f.Close()
	app := App{ID: id}
	inEntry, hidden := false, false
	tryExec := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inEntry = line == "[Desktop Entry]"
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !inEntry || !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Type":
			hidden = hidden || strings.TrimSpace(v) != "Application"
		case "Name":
			app.Name = v
		case "GenericName":
			app.Generic = v
		case "Exec":
			app.Exec = v
		case "TryExec":
			tryExec = v
		case "Keywords":
			app.Keywords = v
		case "NoDisplay", "Hidden":
			hidden = hidden || v == "true"
		}
	}
	if tryExec != "" {
		if _, err := exec.LookPath(tryExec); err != nil {
			return App{}, false
		}
	}
	return app, !hidden && app.Exec != "" && app.Name != ""
}

// Find resolves a user-supplied name ("firefox", "Files", "org.gnome.Nautilus")
// to an installed app, preferring exact matches over partial ones.
func Find(query string) (App, bool) {
	q := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(query, ".desktop")))
	all := List()
	rank := func(a App) int {
		id, name, bin := strings.ToLower(a.ID), strings.ToLower(a.Name), execName(a.Exec)
		switch {
		case id == q || name == q:
			return 0
		case bin == q || strings.HasSuffix(id, "."+q) || strings.HasPrefix(id, q+"_"):
			return 1
		case strings.Contains(name, q) || strings.Contains(id, q):
			return 2
		case strings.Contains(strings.ToLower(a.Generic), q) || strings.Contains(strings.ToLower(a.Keywords), q):
			return 3
		}
		return -1
	}
	best, bestRank := App{}, 99
	for _, a := range all {
		if r := rank(a); r >= 0 && r < bestRank {
			best, bestRank = a, r
		}
	}
	return best, bestRank < 99
}

// Owns reports whether a window app id belongs to this app. Wayland app ids
// are usually the desktop id ("org.gnome.Nautilus"), snaps add a suffix
// ("firefox_firefox"), and X11 classes are often the binary name.
func (a App) Owns(appID string) bool {
	id := strings.ToLower(appID)
	if id == "" {
		return false
	}
	short := id[strings.LastIndex(id, ".")+1:]
	snap, _, _ := strings.Cut(id, "_")
	for _, c := range a.names() {
		if c == id || c == short || c == snap {
			return true
		}
	}
	return false
}

// names lists the lowercase identifiers a window of this app may report.
func (a App) names() []string {
	id := strings.ToLower(a.ID)
	return []string{id, strings.ToLower(a.Name), id[strings.LastIndex(id, ".")+1:], a.Binary()}
}

// Binary is the program the app runs, e.g. "firefox".
func (a App) Binary() string { return execName(a.Exec) }

// execName is the program an Exec line runs, skipping an `env VAR=x` prefix.
func execName(execLine string) string {
	argv := ParseExec(execLine)
	for len(argv) > 0 && (filepath.Base(argv[0]) == "env" || strings.Contains(argv[0], "=")) {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return ""
	}
	return strings.ToLower(filepath.Base(argv[0]))
}

// ParseExec splits a desktop entry Exec line into argv, dropping field codes.
func ParseExec(execLine string) []string {
	var (
		argv          []string
		cur           strings.Builder
		inQuote, have bool
	)
	flush := func() {
		if have {
			argv = append(argv, cur.String())
		}
		cur.Reset()
		have = false
	}
	rs := []rune(execLine)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '"':
			inQuote, have = !inQuote, true
		case r == '\\' && i+1 < len(rs):
			i++
			cur.WriteRune(rs[i])
			have = true
		case r == ' ' && !inQuote:
			flush()
		case r == '%' && i+1 < len(rs):
			i++
			if rs[i] == '%' {
				cur.WriteRune('%')
				have = true
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	flush()
	return argv
}

// Launch starts argv detached from our process tree with env.
func Launch(argv, env []string) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("%s is not installed or not on PATH", argv[0])
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	home, _ := os.UserHomeDir()
	p, err := os.StartProcess(path, argv, &os.ProcAttr{
		Dir:   home,
		Env:   env,
		Files: []*os.File{devnull, devnull, devnull},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		return err
	}
	go p.Wait() // reap if it exits while we live
	return nil
}
