---
name: setup
description: Check whether this Linux machine is ready for computer use and install what is missing (the headless agent desktop, a browser, the GNOME Shell extension), and optionally stop Claude Code from prompting on every computer-use action. Use when the user runs /linux-use:setup, asks why computer use isn't working on Linux, or wants Claude to control apps on a server without a desktop.
argument-hint: ""
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/bin/linux-use *), Read, Edit
---

# linux-use setup

1. Run `${CLAUDE_PLUGIN_ROOT}/bin/linux-use doctor` and read the report.
2. Explain the result to the user in a few lines:
   - Which session Claude will drive: their own desktop (Wayland or X11), or the **agent desktop**, a private headless desktop that linux-use starts for servers and SSH sessions.
   - Which backend drives it, and whether the test screenshot worked.
   - On GNOME, if the doctor says screenshots fell back to the screenshot portal, PipeWire capture failed: GNOME will ask once to allow screenshots and flash the screen on each one. Suggest checking that PipeWire runs (`systemctl --user status pipewire`) and show the error it printed.
3. If the doctor prints an install command, show it and ask before running it, because it needs `sudo`. If they agree, run it, then run the doctor again to confirm.
4. GNOME Shell extension. If the doctor has a "GNOME Shell extension" section that isn't ✓:
   - Explain what it adds. GNOME only lets its own components list windows, so linux-use needs a tiny extension of its own for that. With it, Claude can list and switch windows, and `open_application` brings an app that's already running to the front. It also lets linux-use see which app is in front, so Claude can't type into a terminal or into an app they didn't grant. Without it, the `request_access` grant is the only check. It goes in `~/.local/share/gnome-shell/extensions/linux-use@stivenkn.github.io`, and `linux-use gnome-extension uninstall` removes it.
   - Ask before installing. If they agree, or if the doctor says the installed copy needs updating, run `${CLAUDE_PLUGIN_ROOT}/bin/linux-use gnome-extension install` and relay what it prints.
   - Unless it printed "active", tell them to log out and back in once: GNOME only loads newly installed extensions at login. Logging out closes their apps, including Claude Code if it runs in a terminal there, so they can pick a convenient moment.
   - If the doctor says user extensions are turned off, tell them how to turn them on (the Extensions app, or the `gsettings` command it prints), but don't run that yourself: it turns on all their other extensions too.
5. If no browser is installed and they want web tasks, suggest their distro's package (e.g. `sudo snap install firefox` on Ubuntu, `sudo apt install firefox-esr` on Debian, `sudo dnf install firefox` on Fedora).
6. Permission prompts. Claude Code asks before every MCP tool call unless it's allowed, which means a prompt for every click. Ask the user if they want the computer-use tools allowed without prompting. If yes, add `"mcp__plugin_linux-use_computer-use"` to `permissions.allow` in `~/.claude/settings.json`: read the file, then merge the entry into the existing list (don't overwrite other settings). Mention that on their own desktop, `request_access` still gates which apps Claude may touch. If they want to approve each app grant themselves, allow the individual tools instead and leave `request_access` out.
7. Finish by telling them how to watch Claude work. `/linux-use:desktop` starts the agent desktop and prints its VNC address. On their own desktop, they just watch the screen. Ask them to restart Claude Code if the tools aren't loaded yet.

Don't change system settings beyond installing, with consent, the packages and the GNOME Shell extension the doctor lists.
