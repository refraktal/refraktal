package acptest

// Self-expiring process trees for kill-path tests. No cleanup here ever
// signals a PID discovered from a listing, a file, or a process name.
import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const tortureEnv = "ACPTEST_TORTURE"

// TortureSpec travels unchanged to every generation. Slots are permanent:
// failed launches also consume one, and no descendant gets a fresh budget
// or a later deadline. The bomb uses no shell or other subprocess launcher.
type TortureSpec struct {
	Dir      string
	Deadline time.Time
	Cap      int
	Width    int
	Depth    int
}

type tortureNode struct {
	TortureSpec
	Role  string
	Slot  int
	Depth int
	// Launcher is the PID of the process that started this one. A daemon
	// must not use getppid: its double-fork parent may already have exited,
	// and it would record init (1) as its parent.
	Launcher int
}

// TortureProcess is written by the process itself before its ready marker.
// Birth identities must still be obtained by the caller before any signal.
type TortureProcess struct {
	PID, Parent int
	Role        string
}

// claimSlot bounds total launches across all generations and all races.
// Never unlink a slot, even if Start fails.
func claimSlot(s TortureSpec) (int, error) {
	if s.Cap < 1 || s.Cap > 256 || time.Until(s.Deadline) <= 0 || time.Until(s.Deadline) > 45*time.Second {
		return 0, fmt.Errorf("invalid torture cap or deadline")
	}
	for i := 0; i < s.Cap; i++ {
		f, err := os.OpenFile(filepath.Join(s.Dir, fmt.Sprintf("slot-%03d", i)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		return i, f.Close()
	}
	return 0, fmt.Errorf("torture launch cap reached")
}

// TortureCommand reserves the harness's slot too. Every process has the
// unique fixture directory in argv, allowing read-only survivor checks even
// for a process killed before it could write its PID file.
func TortureCommand(s TortureSpec, role string) (argv, env []string, err error) {
	slot, err := claimSlot(s)
	if err != nil {
		return nil, nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	n := tortureNode{TortureSpec: s, Role: role, Slot: slot, Depth: s.Depth}
	b, err := json.Marshal(n)
	return []string{self, "acptest-torture", s.Dir}, []string{tortureEnv + "=" + string(b),
		envMode + "=", "GORACE=atexit_sleep_ms=0"}, err
}

// RunIfTorture runs before m.Run in a test binary, after MaybeShellPrefix.
func RunIfTorture() {
	v := os.Getenv(tortureEnv)
	if v == "" {
		return
	}
	var n tortureNode
	if json.Unmarshal([]byte(v), &n) != nil || n.Cap < 1 || n.Cap > 256 || n.Width < 0 || n.Width > 16 ||
		n.Depth < 0 || n.Depth > 12 || n.Slot < 0 || n.Slot >= n.Cap {
		os.Exit(125)
	}
	left := time.Until(n.Deadline)
	if left <= 0 || left > 45*time.Second {
		os.Exit(125)
	}
	// Runs even if the helper blocks in a read, a write, or a child's Wait.
	time.AfterFunc(left, func() { os.Exit(124) })
	if n.Role == "closedio" || n.Role == "stubborn" {
		signal.Ignore(syscall.SIGTERM)
	}
	b, _ := json.Marshal(TortureProcess{PID: os.Getpid(), Parent: cmp.Or(n.Launcher, os.Getppid()), Role: n.Role})
	if os.WriteFile(filepath.Join(n.Dir, fmt.Sprintf("pid-%03d.json", n.Slot)), b, 0o600) != nil {
		os.Exit(125)
	}
	if err := n.run(); err != nil {
		// A short code only; never dump inherited environment or argv.
		os.WriteFile(filepath.Join(n.Dir, "error"), []byte("fake failed\n"), 0o600)
		os.Exit(125)
	}
	os.Exit(0)
}

func (n tortureNode) mark(name string) error {
	return os.WriteFile(filepath.Join(n.Dir, name), nil, 0o600)
}

func (n tortureNode) await(name string) error {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for time.Now().Before(n.Deadline) {
		if _, err := os.Stat(filepath.Join(n.Dir, name)); err == nil {
			return nil
		}
		<-t.C
	}
	return fmt.Errorf("fake gate expired")
}

func (n tortureNode) spawn(role string, depth int, attr *syscall.SysProcAttr, prefix bool) (*exec.Cmd, error) {
	slot, err := claimSlot(n.TortureSpec)
	if err != nil {
		return nil, err
	}
	kid := n
	kid.Role, kid.Slot, kid.Depth, kid.Launcher = role, slot, depth, os.Getpid()
	b, _ := json.Marshal(kid)
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, "acptest-torture", n.Dir)
	// The prefix execs the shell, which immediately execs the test binary:
	// these are one launch, with one PID and one reserved slot.
	if prefix {
		cmd = exec.Command(self, "exec "+shellQuote(self)+" acptest-torture "+shellQuote(n.Dir))
	}
	cmd.Env = append(os.Environ(), tortureEnv+"="+string(b), envMode+"=", "REFRAKTAL_HELPER=", "GORACE=atexit_sleep_ms=0")
	if prefix {
		cmd.Env = append(cmd.Env, "REFRAKTAL_HELPER=shell-prefix", "REFRAKTAL_SESSIONS="+filepath.Join(n.Dir, "sessions"),
			"REFRAKTAL_PREFIX_NEXT=", "REFRAKTAL_PREFIX_VAR=CLAUDE_CODE_SHELL_PREFIX", "CLAUDE_CODE_SHELL_PREFIX="+self,
			"CLAUDE_CODE_SHELL=/bin/bash", "SHELL=/bin/bash", "BASH_ENV=", "ENV=")
	}
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func (n tortureNode) launch(role string, depth int, attr *syscall.SysProcAttr, prefix bool) error {
	cmd, err := n.spawn(role, depth, attr, prefix)
	if err == nil {
		go cmd.Wait() // reap direct children while this parent is alive
	}
	return err
}

func (n tortureNode) run() error {
	switch n.Role {
	case "cycle":
		if err := n.launch("leaf", 0, &syscall.SysProcAttr{Setpgid: true}, false); err != nil {
			return err
		}
		if err := n.mark("ready"); err != nil {
			return err
		}
	case "tree":
		for i := 0; i < n.Width; i++ {
			if err := n.launch("branch", n.Depth, &syscall.SysProcAttr{Setpgid: true}, false); err != nil {
				return err
			}
		}
		// Two forks detach a daemon inside the harness session. Another two
		// forks run outside it, through the actual shell-prefix helper.
		if err := n.launch("daemon-parent", 0, &syscall.SysProcAttr{Setpgid: true}, false); err != nil {
			return err
		}
		if err := n.launch("daemon-parent", 0, &syscall.SysProcAttr{Setsid: true}, true); err != nil {
			return err
		}
		return n.markAndWait()
	case "branch":
		if n.Depth > 1 {
			if err := n.launch("branch", n.Depth-1, nil, false); err != nil {
				return err
			}
		}
	case "daemon-parent":
		cmd, err := n.spawn("daemon-middle", 0, nil, false)
		if err != nil {
			return err
		}
		return cmd.Wait()
	case "daemon-middle":
		return n.launch("daemon", 0, &syscall.SysProcAttr{Setpgid: true}, false)
	case "bomb":
		if err := n.await("start"); err != nil {
			return err
		}
		cmd, err := n.spawn("bomb-parent", 0, nil, false)
		if err != nil {
			return err
		}
		if err := cmd.Wait(); err != nil {
			return err
		}
		return n.mark("ready") // exit before the tracker can see the lineage
	case "bomb-parent":
		return n.launch("bomb-seed", 0, &syscall.SysProcAttr{Setpgid: true}, false)
	case "bomb-seed", "bomb-child":
		if err := n.await("fork"); err != nil {
			return err
		}
		for i := 0; i < 2; i++ {
			// Exhaustion is normal. Failed starts still spend a permanent slot.
			if err := n.launch("bomb-child", 0, &syscall.SysProcAttr{Setpgid: true}, false); err != nil {
				break
			}
		}
	case "closedio":
		if err := n.launch("stubborn", 0, &syscall.SysProcAttr{Setpgid: true}, false); err != nil {
			return err
		}
		// Confirm that a request is pending before closing both pipes. The
		// child has /dev/null stdio, so it cannot hold EOF open.
		if _, err := bufio.NewReader(os.Stdin).ReadBytes('\n'); err != nil {
			return err
		}
		if err := n.mark("request-read"); err != nil {
			return err
		}
		if err := n.await("close-stdio"); err != nil {
			return err
		}
		os.Stdin.Close()
		os.Stdout.Close()
		if err := n.mark("closed-stdio"); err != nil {
			return err
		}
	case "agent-tool", "agent-tool-change", "agent-edit", "agent-edit-tree", "agent-judge":
		Serve(n.Role, os.Stdin, os.Stdout)
		return nil
	case "leaf":
		if err := n.mark(fmt.Sprintf("leaf-ready-%d", n.Launcher)); err != nil {
			return err
		}
	case "daemon", "stubborn":
	default:
		return fmt.Errorf("unknown torture role")
	}
	return n.awaitExpiry() // no polling or work while waiting for the safety expiry
}

// readyTree starts a child in another process group and waits until its own
// child has recorded its PID. A prompt cannot finish before both exist.
func (n tortureNode) readyTree() error {
	cmd, err := n.spawn("cycle", 0, &syscall.SysProcAttr{Setpgid: true}, false)
	if err != nil {
		return err
	}
	go cmd.Wait()
	return n.await(fmt.Sprintf("leaf-ready-%d", cmd.Process.Pid))
}

func (n tortureNode) markAndWait() error {
	if err := n.mark("ready"); err != nil {
		return err
	}
	return n.awaitExpiry()
}

// The lifetime timer is a safety bound, not readiness synchronization.
func (n tortureNode) awaitExpiry() error {
	timer := time.NewTimer(time.Until(n.Deadline))
	defer timer.Stop()
	<-timer.C
	return nil
}
