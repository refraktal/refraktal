package acptest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	RunIfTorture()
	os.Exit(m.Run())
}

// This runs without ps or a killer. A failed test cannot prolong the fake:
// its own absolute watchdog must terminate it, even with stdin left open.
func TestTortureFakeSelfExpires(t *testing.T) {
	s := NewTortureSpec(t, false)
	s.Deadline = time.Now().Add(time.Second)
	argv, env, err := TortureCommand(s, "leaf")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 124 {
				t.Fatalf("fake exited for a reason other than safety expiry: %v", err)
			}
		}
		if time.Now().Before(s.Deadline) || time.Since(s.Deadline) > 2*time.Second {
			t.Fatal("fake did not respect its own absolute expiry")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("fake did not self-expire; no process was signalled")
	}
}

func TestTorturePermanentSlotsBoundConcurrentLaunches(t *testing.T) {
	for _, heavy := range []bool{false, true} {
		s := NewTortureSpec(t, heavy)
		var wg sync.WaitGroup
		var mu sync.Mutex
		claimed := map[int]bool{}
		for worker := 0; worker < 16; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					slot, err := claimSlot(s)
					if err != nil {
						return
					}
					mu.Lock()
					if claimed[slot] {
						t.Errorf("slot %d allocated twice", slot)
					}
					claimed[slot] = true
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(claimed) != s.Cap || TortureSlots(t, s) != s.Cap {
			t.Fatalf("allocated %d slots, want exactly %d", len(claimed), s.Cap)
		}
		// Exhaustion is permanent, even when none of these reservations has
		// launched a process. Failed or exited children cannot refill it.
		if _, err := claimSlot(s); err == nil {
			t.Fatal("exhausted launch budget refilled")
		}
	}
}

func TestTortureRejectsUnsafeCapAndExpiry(t *testing.T) {
	for _, s := range []TortureSpec{
		{Cap: 0, Deadline: time.Now().Add(time.Second)},
		{Cap: 257, Deadline: time.Now().Add(time.Second)},
		{Cap: 64, Deadline: time.Now().Add(-time.Second)},
		{Cap: 64, Deadline: time.Now().Add(time.Minute)},
	} {
		s.Dir = t.TempDir()
		if _, err := claimSlot(s); err == nil {
			t.Fatal("unsafe launch spec accepted")
		}
		entries, err := os.ReadDir(s.Dir)
		if err != nil || len(entries) != 0 {
			t.Fatal("invalid spec consumed a launch slot")
		}
	}
}

func TestTortureCommandPreservesSharedBudget(t *testing.T) {
	s := NewTortureSpec(t, false)
	argv, env, err := TortureCommand(s, "bomb")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(argv[0]) || argv[len(argv)-1] != s.Dir {
		t.Fatal("fake argv lacks its unique fixture marker")
	}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, tortureEnv+"="); ok {
			var n tortureNode
			if err := json.Unmarshal([]byte(v), &n); err != nil || n.Dir != s.Dir || n.Cap != s.Cap || !n.Deadline.Equal(s.Deadline) || n.Slot != 0 {
				t.Fatal("command changed the shared cap or absolute deadline")
			}
			return
		}
	}
	t.Fatal("missing fake spec")
}
