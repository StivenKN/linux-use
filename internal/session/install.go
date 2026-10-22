package session

import (
	"bufio"
	"os"
	"strings"
)

// DesktopPackages are the programs the agent desktop uses; labwc is required,
// the rest degrade gracefully.
var DesktopPackages = []string{"labwc", "Xwayland", "wayvnc", "waybar", "swaybg", "foot"}

// InstallCommand returns the package-manager command that installs the given
// programs on this distro.
func InstallCommand(programs []string) string {
	rel := osRelease()
	id := rel["ID"] + " " + rel["ID_LIKE"]
	prefix, xwayland := "sudo apt install -y --no-install-recommends", "xwayland"
	switch {
	case strings.Contains(id, "fedora") || strings.Contains(id, "rhel"):
		prefix, xwayland = "sudo dnf install -y", "xorg-x11-server-Xwayland"
	case strings.Contains(id, "arch"):
		prefix, xwayland = "sudo pacman -S --needed", "xorg-xwayland"
	case strings.Contains(id, "suse"):
		prefix = "sudo zypper install -y"
	case strings.Contains(id, "alpine"):
		prefix = "sudo apk add"
	}
	pkgs := make([]string, len(programs))
	for i, p := range programs {
		pkgs[i] = p
		if p == "Xwayland" {
			pkgs[i] = xwayland
		}
	}
	return prefix + " " + strings.Join(pkgs, " ")
}

func osRelease() map[string]string {
	m := map[string]string{}
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			m[k] = strings.Trim(v, `"`)
		}
	}
	return m
}
