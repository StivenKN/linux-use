---
name: desktop
description: Start, stop, or check the linux-use agent desktop (a headless Wayland desktop Claude drives on servers and over SSH) and explain how to watch it over VNC. Use when the user runs /linux-use:desktop, or asks to see, watch, restart or reset the desktop Claude is using.
argument-hint: "[up|down|status|restart] [--size 1280x800]"
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/bin/linux-use *)
---

# linux-use agent desktop

Arguments: $ARGUMENTS (default: `up`).

- `up` (optionally with `--size WxH` or `--window` to open it as a window inside the user's own desktop): run `${CLAUDE_PLUGIN_ROOT}/bin/linux-use desktop up <args>`.
- `down`: run `${CLAUDE_PLUGIN_ROOT}/bin/linux-use desktop down`. This closes every app inside it.
- `restart`: run `down` then `up`.
- `status`: run `${CLAUDE_PLUGIN_ROOT}/bin/linux-use desktop status`.

After `up` or `status`, tell the user how to watch, using the VNC address the command printed:
- On the same machine: open `vnc://<address>` in a VNC viewer (TigerVNC, Remmina, RealVNC; macOS Screen Sharing also works).
- From another machine: tunnel first with `ssh -L 5900:<address> <user>@<host>`, then open `vnc://localhost:5900`.
- The viewer is interactive, so the user can take over (log in, solve a CAPTCHA) and hand back.

The agent desktop starts on its own the first time a computer-use tool needs it, so this command is optional.
