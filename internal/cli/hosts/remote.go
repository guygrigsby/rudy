package hosts

import "strings"

// ExitNoRudy is what the remote line exits when rudy is not on the box's PATH. Outside
// sysexits and the shell's 126/127, and the same number sand picks its codes from, so the
// client can tell "install rudy" from every other failure ssh reports.
const ExitNoRudy = 111

// remotePath is prepended on the box before anything is looked up. ssh box '<cmd>' runs a
// non-interactive shell with the compiled-in PATH, so a rudy under $HOME is invisible
// without it.
const remotePath = `PATH="$HOME/.local/bin:$HOME/bin:$HOME/go/bin:$PATH"`

// RemoteLine is the one shell line ssh runs on the box: fix PATH, say plainly when rudy is
// absent, exec the bridge with args.
func RemoteLine(args ...string) string {
	cmd := "exec rudy bridge"
	if len(args) > 0 {
		cmd += " " + strings.Join(args, " ")
	}
	return remotePath + "; command -v rudy >/dev/null 2>&1 || exit 111; " + cmd
}
