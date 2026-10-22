package computer

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/StivenKN/linux-use/internal/session"
)

// lock makes sure only one Claude session drives a desktop at a time. It is
// an flock, so it is released automatically when the holder exits.
func lock(s session.Session) (*os.File, error) {
	dir := session.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, s.Key()+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		b, _ := os.ReadFile(f.Name())
		f.Close()
		holder := strings.TrimSpace(string(b))
		if _, err := strconv.Atoi(holder); err != nil {
			holder = "unknown"
		}
		return nil, fmt.Errorf("computer use is in use by another Claude session (pid %s). Wait for that session to finish or run /exit there", holder)
	}
	f.Truncate(0)
	f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	return f, nil
}
