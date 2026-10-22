---
name: desktop
description: Start, stop, or check the linux-use agent desktop (a private headless Wayland desktop Claude drives on servers and over SSH), and explain how to watch it over VNC. Use when the user runs /linux-use:desktop, or asks to see, watch, restart or reset the desktop Claude is using.
argument-hint: "[up|down|status|restart] [--size 1280x800] [--window] [--vnc-listen 127.0.0.1]"
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/bin/linux-use *)
---

# linux-use agent desktop

Arguments: $ARGUMENTS (default: `up`).

- `up`: run `${CLAUDE_PLUGIN_ROOT}/bin/linux-use desktop up <args>`. Useful options:
  - `--size WxH`: screen size (default 1280x800).
  - `--window`: show it as a window inside the user's own desktop instead of headless.
  - `--vnc-listen 127.0.0.1`: serve the viewer on TCP instead of a private unix socket.
- `down`: run `${CLAUDE_PLUGIN_ROOT}/bin/linux-use desktop down`. This closes every app inside it.
- `restart`: run `down`, then `up`.
- `status`: run `${CLAUDE_PLUGIN_ROOT}/bin/linux-use desktop status`.

After `up` or `status`, relay the "watch it" command the tool printed, and explain:
- The user runs that `ssh -L …` on their laptop, then opens `vnc://localhost:5900` in a VNC viewer (macOS Screen Sharing, TigerVNC, Remmina, RealVNC).
- By default the viewer is a unix socket only their user can reach. If their SSH server refuses socket forwarding (Tailscale SSH does when logging in as another user), restart with `--vnc-listen 127.0.0.1` and tunnel `5900:127.0.0.1:5900` instead. Warn them that the TCP viewer has no password, so any local user on that machine could connect.
- The viewer is interactive: they can take over (logins, 2FA, CAPTCHAs) and hand back.

The agent desktop also starts on its own the first time a computer-use tool needs it, so running this is optional.
