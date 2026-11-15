// Package proc spawns harness processes and makes sure nothing they start
// outlives them.
//
// Harness tools run in process groups of their own, so a process-group kill
// does not reach them. A Tracker therefore records every descendant of the
// harness by parent PID while it runs, and Kill stops tracked processes by PID
// after checking that each one is still the process that was recorded.
package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Proc identifies one process by its birth identity: PID and start time. A
// PID alone is not an identity, because PIDs are reused. The command line is
// for display and logs only: it changes when a process execs.
type Proc struct {
	PID   int    `json:"pid"`
	PPID  int    `json:"ppid"`
	PGID  int    `json:"pgid"`
	UID   int    `json:"uid"`
	Start string `json:"start"` // start time as ps prints it (lstart), one-second resolution
	Cmd   string `json:"cmd"`   // command line; it holds local paths, so never log it verbatim
}

// Same reports whether p and q are the same process: same PID and start
// time. (A PID reused within the same second of ps's one-second resolution
// would look the same; that is the accepted limit.)
func (p Proc) Same(q Proc) bool {
	return p.PID == q.PID && p.Start == q.Start
}

// List returns every running process on the machine, by PID. Zombies and,
// on macOS, processes already exiting (state flag E, shown with the command
// in parentheses) are left out: they cannot be killed and their command line
// no longer matches the record.
func List() (map[int]Proc, error) {
	// These flags work with both the BSD ps on macOS and procps on Linux. -ww
	// stops the command line being cut to the terminal width; LC_ALL=C fixes
	// the lstart format to five fields, such as "Sun Nov 15 15:45:51 2026".
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-A", "-ww", "-o", "pid=", "-o", "ppid=", "-o", "pgid=", "-o", "uid=", "-o", "stat=",
		"-o", "lstart=", "-o", "command=")
	cmd.Env = append(ScrubEnv(os.Environ()), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return parsePS(string(out))
}

func parsePS(out string) (map[int]Proc, error) {
	all := map[int]Proc{}
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 11 { // pid ppid pgid uid stat, five lstart fields, at least one command field
			return nil, fmt.Errorf("ps: malformed row %d", i+1)
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		pgid, err3 := strconv.Atoi(f[2])
		uid, err4 := strconv.Atoi(f[3])
		start := strings.Join(f[5:10], " ")
		_, validStart := startTime(start)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || pid <= 0 || ppid < 0 || pgid < 0 || uid < 0 ||
			!strings.ContainsRune("DIPRSTtUWXZ?", rune(f[4][0])) || !validStart {
			return nil, fmt.Errorf("ps: malformed row %d", i+1)
		}
		if strings.HasPrefix(f[4], "Z") || strings.Contains(f[4], "E") {
			continue
		}
		all[pid] = Proc{PID: pid, PPID: ppid, PGID: pgid, UID: uid,
			Start: start, Cmd: strings.Join(f[10:], " ")}
	}
	return all, nil
}
