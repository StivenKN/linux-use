// linux-use GNOME Shell extension (GNOME 45+). GNOME only lets its own
// components list windows (org.gnome.Shell.Introspect is allowlisted), so
// this exports a tiny D-Bus interface on the shell's own connection, reached
// through the org.gnome.Shell name, that lets linux-use list and focus
// windows. `linux-use gnome-extension install` installs it.
import GLib from 'gi://GLib';
import Gio from 'gi://Gio';
import Meta from 'gi://Meta';
import Shell from 'gi://Shell';
import * as Main from 'resource:///org/gnome/shell/ui/main.js';
import {Extension} from 'resource:///org/gnome/shell/extensions/extension.js';

// Bump with every change, together with extVersion in extension.go.
const VERSION = 1;

const IFACE = `<node><interface name="io.github.stivenkn.LinuxUse">
  <method name="Windows"><arg name="windows" type="a(tssbb)" direction="out"/></method>
  <method name="Activate"><arg name="id" type="t" direction="in"/></method>
  <property name="Version" type="u" access="read"/>
</interface></node>`;

const TYPES = [Meta.WindowType.NORMAL, Meta.WindowType.DIALOG, Meta.WindowType.MODAL_DIALOG];

// windows lists app windows on every workspace in creation order: normal
// windows and dialogs, plus whichever window has focus, so linux-use always
// sees where keys go.
function windows() {
    const focus = global.display.focus_window;
    return global.get_window_actors().map(a => a.meta_window).filter(w =>
        !w.is_override_redirect() &&
        (w === focus || (!w.is_skip_taskbar() && TYPES.includes(w.get_window_type()))))
        .sort((a, b) => a.get_id() - b.get_id());
}

const service = {
    Version: VERSION,

    // Windows returns (id, title, app id, focused, minimized) per window. The
    // app id is GNOME's desktop file id ("org.gnome.Nautilus.desktop"), or
    // the WM class when GNOME matched the window to no app.
    Windows() {
        const tracker = Shell.WindowTracker.get_default();
        const focus = global.display.focus_window;
        return windows().map(w => {
            const app = tracker.get_window_app(w);
            const id = app && !app.is_window_backed() ? app.get_id() : w.get_wm_class();
            return [w.get_id(), w.get_title() ?? '', id ?? '', w === focus, w.minimized];
        });
    },

    // Activate switches to the window's workspace, unminimizes and focuses it.
    Activate(id) {
        const win = windows().find(w => w.get_id() === id);
        if (!win)
            throw new GLib.Error(Gio.DBusError, Gio.DBusError.INVALID_ARGS, `no window with id ${id}`);
        Main.activateWindow(win);
    },
};

export default class LinuxUseExtension extends Extension {
    enable() {
        this._dbus = Gio.DBusExportedObject.wrapJSObject(IFACE, service);
        this._dbus.export(Gio.DBus.session, '/io/github/stivenkn/LinuxUse');
    }

    disable() {
        this._dbus.unexport();
        this._dbus = null;
    }
}
