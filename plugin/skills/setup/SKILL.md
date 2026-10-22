---
name: setup
description: Check whether this Linux machine is ready for computer use and install what is missing (the headless agent desktop, a browser). Use when the user runs /linux-use:setup, asks why computer use is not working on Linux, or wants Claude to control apps on a server without a desktop.
argument-hint: ""
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/bin/linux-use *)
---

# linux-use setup

1. Run `${CLAUDE_PLUGIN_ROOT}/bin/linux-use doctor` and read the report.
2. Explain the result to the user in a few lines:
   - Which session Claude will drive: their own desktop (Wayland or X11), or the **agent desktop**, a private headless desktop that linux-use starts for servers and SSH sessions.
   - Which backend drives it and whether the test screenshot worked.
3. If the doctor prints an install command, show it and ask before running it, because it needs `sudo`. If they agree, run it, then run the doctor again to confirm.
4. If no browser is installed and they want web tasks, suggest their distro's browser package (e.g. `sudo snap install firefox` on Ubuntu, `sudo apt install firefox-esr` on Debian, `sudo dnf install firefox` on Fedora).
5. Finish by telling them how to watch Claude work: `/linux-use:desktop` starts the agent desktop and prints the VNC address. On their own desktop, they just watch the screen.

Do not change system settings beyond installing the packages the doctor lists.
