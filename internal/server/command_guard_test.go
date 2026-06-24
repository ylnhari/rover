package server

import "testing"

func TestCommandGuardBlocks(t *testing.T) {
	blocked := []string{
		"vim notes.txt", "top", "less file.txt", "man ls",
		"ssh host", "passwd", "su -",
		"python", "node", "psql", "PORT=3 python",
		"git commit", "git rebase -i", "npm init",
		"cd /tmp", "export FOO=bar", "source venv/bin/activate", "pushd x",
		"code .", "chrome https://example.com", "notepad", "xdg-open file.pdf",
		"tail -f server.log",
	}
	for _, c := range blocked {
		if ok, _ := commandGuard(c); !ok {
			t.Errorf("expected BLOCK but allowed: %q", c)
		}
	}
}

func TestCommandGuardAllows(t *testing.T) {
	allowed := []string{
		"ls -la", "go test ./...", "make build", "echo hi",
		"git status", "git commit -m hi", "git rebase main",
		"python script.py", "node app.js", "npm init -y",
		"cd /tmp && ls", "code --version", "code -v",
		"npm run dev", "npm start", "vite", // long-running: allowed
		"tail -n 50 log", "pytest -q",
	}
	for _, c := range allowed {
		if ok, r := commandGuard(c); ok {
			t.Errorf("expected ALLOW but blocked: %q (%s)", c, r)
		}
	}
}
