package server

import (
	"regexp"
	"strings"
)

// rover runs each Terminal command as a fresh, non-interactive process on the
// host (cmd /C / sh -c) with no stdin, no TTY, and no state carried between
// commands. commandGuard rejects commands that therefore cannot work — they
// would hang until the timeout, do nothing useful, or only act on the host.
//
// It is a heuristic on the first token (after a leading sudo / VAR=val prefix),
// not a parser; long-running servers/watchers are intentionally NOT blocked.

var reSudoEnvPrefix = regexp.MustCompile(`^(sudo\s+)?((\w+=\S+)\s+)*`)

func strSet(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

var (
	guardEditors = strSet("vim", "vi", "nano", "emacs", "pico", "micro", "vimtutor")
	guardPagers  = strSet("less", "more", "man", "top", "htop", "btop")
	guardPrompts = strSet("ssh", "telnet", "ftp", "sftp", "su", "passwd", "gpg")
	guardRepls   = strSet("python", "python3", "node", "ruby", "irb", "php", "psql",
		"mysql", "sqlite3", "mongo", "mongosh", "redis-cli", "bash", "sh", "zsh",
		"fish", "pwsh", "powershell", "julia", "gdb")
	guardGUI = strSet("start", "explorer", "open", "xdg-open", "gnome-open", "kde-open",
		"code", "notepad", "notepad++", "gedit", "chrome", "google-chrome", "chromium",
		"firefox", "msedge", "edge", "safari", "brave")
)

// commandGuard reports whether a command should be blocked, with a reason.
func commandGuard(command string) (bool, string) {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return false, ""
	}
	rest := reSudoEnvPrefix.ReplaceAllString(cmd, "")
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return false, ""
	}
	prog := strings.ToLower(fields[0])
	if i := strings.LastIndexAny(prog, `/\`); i >= 0 {
		prog = prog[i+1:]
	}
	for _, ext := range []string{".exe", ".cmd", ".bat"} {
		prog = strings.TrimSuffix(prog, ext)
	}
	args := fields[1:]
	lc := strings.ToLower(rest)
	chained := strings.ContainsAny(rest, "&;|")
	hasAny := func(opts ...string) bool {
		for _, o := range opts {
			for _, a := range args {
				if a == o {
					return true
				}
			}
		}
		return false
	}

	switch {
	case guardEditors[prog]:
		return true, prog + " is an interactive editor; rover has no terminal or stdin, so it would hang until the timeout."
	case guardPagers[prog]:
		return true, prog + " needs an interactive terminal, which rover does not provide; it would hang until the timeout."
	case guardPrompts[prog]:
		return true, prog + " waits for input (e.g. a password or confirmation); rover cannot send input, so it would hang. Use a non-interactive form (keys/flags)."
	case guardGUI[prog]:
		if hasAny("--version", "-v", "--help", "-h") {
			return false, ""
		}
		return true, prog + " opens a window on the rover host, not in your browser; it is not usable through rover."
	case prog == "tail" && hasAny("-f", "--follow"):
		return true, "tail -f never ends and a Terminal command cannot be stopped; it would run until the timeout / output limit kills it."
	case guardRepls[prog] && len(args) == 0:
		return true, prog + " with no script starts an interactive REPL; rover has no stdin, so it would hang. Pass a script or use -c."
	case prog == "git" && len(args) > 0 && args[0] == "commit" && !hasAny("-m", "--message", "--amend", "-F", "--file"):
		return true, "git commit without -m opens an editor, which would hang. Use git commit -m \"message\"."
	case prog == "git" && len(args) > 0 && args[0] == "rebase" && hasAny("-i", "--interactive"):
		return true, "git rebase -i is interactive and would hang; rover has no terminal."
	case prog == "npm" && len(args) > 0 && args[0] == "init" && !hasAny("-y", "--yes"):
		return true, "npm init prompts interactively; add -y, or it would hang."
	case !chained && (prog == "cd" || prog == "export" || prog == "set" || prog == "source" || prog == "." || prog == "pushd"):
		return true, "rover runs each command in a fresh shell, so this has no effect on later commands. Chain it in one command with &&, e.g. cd dir && your-command."
	case !chained && (strings.Contains(lc, "activate") || strings.HasPrefix(lc, "nvm use") || strings.HasPrefix(lc, "conda activate")):
		return true, "environment activation does not persist to the next command; combine it with && in a single command."
	}
	return false, ""
}
