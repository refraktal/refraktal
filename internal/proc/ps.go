// Package proc spawns harness processes and makes sure nothing they start
// outlives them.
//
// Harness tools run in process groups of their own, so a process-group kill
// does not reach them. A Tracker therefore records every descendant of the
// harness by parent PID while it runs, and Kill stops tracked processes by PID
// after checking that each one is still the process that was recorded.
package proc

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Proc identifies one process. A PID alone is not an identity, because PIDs
// are reused: Start and Cmd must also match.
type Proc struct {
	PID   int    `json:"pid"`
	PPID  int    `json:"ppid"`
	PGID  int    `json:"pgid"`
	Start string `json:"start"` // start time as ps prints it (lstart), one-second resolution
	Cmd   string `json:"cmd"`   // command line; it holds local paths, so never log it verbatim
}

// Same reports whether p and q are the same process: same PID, start time and
// command line.
func (p Proc) Same(q Proc) bool {
	return p.PID == q.PID && p.Start == q.Start && p.Cmd == q.Cmd
}

// List returns every running process on the machine, by PID. Zombies and,
// on macOS, processes already exiting (state flag E, shown with the command
// in parentheses) are left out: they cannot be killed and their command line
// no longer matches the record.
func List() (map[int]Proc, error) {
	// These flags work with both the BSD ps on macOS and procps on Linux. -ww
	// stops the command line being cut to the terminal width; LC_ALL=C fixes
	// the lstart format to five fields, such as "Sun Nov 15 15:45:51 2026".
	cmd := exec.Command("ps", "-A", "-ww", "-o", "pid=", "-o", "ppid=", "-o", "pgid=", "-o", "stat=",
		"-o", "lstart=", "-o", "command=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return parsePS(string(out)), nil
}

func parsePS(out string) map[int]Proc {
	all := map[int]Proc{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 { // pid ppid pgid stat, five lstart fields, at least one command field
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		pgid, err3 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || err3 != nil || strings.HasPrefix(f[3], "Z") || strings.Contains(f[3], "E") {
			continue
		}
		all[pid] = Proc{PID: pid, PPID: ppid, PGID: pgid,
			Start: strings.Join(f[4:9], " "), Cmd: strings.Join(f[9:], " ")}
	}
	return all
}
