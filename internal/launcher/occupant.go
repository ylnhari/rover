package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Occupant identifies the process listening on a port, so users can decide
// what to do about a conflict instead of rover killing blindly.
type Occupant struct {
	PID     int    `json:"pid"`
	Name    string `json:"name,omitempty"`
	CmdLine string `json:"cmdline,omitempty"`
}

func (o *Occupant) String() string {
	desc := fmt.Sprintf("pid %d", o.PID)
	if o.Name != "" {
		desc += " (" + o.Name + ")"
	}
	return desc
}

// FindListenerOnPort identifies the process LISTENING on the given TCP port
// (never a client merely connected to it). Returns nil if none could be
// identified.
func FindListenerOnPort(port int) *Occupant {
	pid := findListenerPID(port)
	if pid <= 0 {
		return nil
	}
	occ := &Occupant{PID: pid}
	occ.Name, occ.CmdLine = processDetails(pid)
	return occ
}

func findListenerPID(port int) int {
	if runtime.GOOS == "windows" {
		// No "-p tcp" filter: it would hide TCPv6 rows, and a dual-stack
		// listener bound to [::]:port occupies the IPv4 port too. Both v4 and
		// v6 rows report proto "TCP", so one parse covers them.
		out, err := exec.Command("netstat", "-ano").Output()
		if err != nil {
			return 0
		}
		suffix := ":" + strconv.Itoa(port)
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			// TCP  <local>  <foreign>  LISTENING  <pid>
			if len(fields) < 5 || !strings.EqualFold(fields[0], "TCP") {
				continue
			}
			if !strings.EqualFold(fields[3], "LISTENING") {
				continue
			}
			if !strings.HasSuffix(fields[1], suffix) {
				continue
			}
			if pid, err := strconv.Atoi(fields[4]); err == nil {
				return pid
			}
		}
		return 0
	}
	// Unix: listener sockets only (-sTCP:LISTEN), never mere clients.
	out, err := exec.Command("lsof", "-nP", "-sTCP:LISTEN", "-ti", "tcp:"+strconv.Itoa(port)).Output()
	if err != nil {
		return 0
	}
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			return pid
		}
	}
	return 0
}

// processDetails returns the process name and command line for a PID
// (best-effort; empty strings when unavailable).
func processDetails(pid int) (name, cmdline string) {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
		if err == nil {
			line := strings.TrimSpace(string(out))
			if strings.HasPrefix(line, "\"") {
				if parts := strings.SplitN(line, "\",\"", 2); len(parts) > 0 {
					name = strings.Trim(parts[0], "\"")
				}
			}
		}
		return name, ""
	}
	if out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output(); err == nil {
		name = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output(); err == nil {
		cmdline = strings.TrimSpace(string(out))
	}
	return name, cmdline
}

// KillConfirmedListener kills the process listening on port, but only if its
// PID still matches the one the caller confirmed (guarding against a different
// process grabbing the port in between) and it is not rover itself.
func KillConfirmedListener(port, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	if pid == os.Getpid() {
		return fmt.Errorf("refusing to kill rover itself (pid %d)", pid)
	}
	current := findListenerPID(port)
	if current != pid {
		return fmt.Errorf("port %d is no longer held by pid %d (now pid %d) — re-check the conflict", port, pid, current)
	}
	if runtime.GOOS == "windows" {
		if err := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run(); err != nil {
			return fmt.Errorf("failed to kill pid %d: %w", pid, err)
		}
		return nil
	}
	if err := exec.Command("kill", "-9", strconv.Itoa(pid)).Run(); err != nil {
		return fmt.Errorf("failed to kill pid %d: %w", pid, err)
	}
	return nil
}
