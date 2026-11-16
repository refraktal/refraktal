package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func procBound(t *testing.T, n int, parse func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	deadline := 2*time.Second + time.Duration(n/(1<<20))*100*time.Millisecond
	watchdog := time.AfterFunc(deadline, func() { panic("process parser exceeded work deadline") })
	defer watchdog.Stop()
	start := time.Now()
	parse()
	elapsed := time.Since(start)
	watchdog.Stop()
	runtime.ReadMemStats(&after)
	if used, limit := after.TotalAlloc-before.TotalAlloc, uint64(n)*64+(1<<20); used > limit {
		t.Fatalf("parser allocated %d bytes (limit %d)", used, limit)
	}
	if elapsed > deadline {
		t.Fatalf("parser took %s for %d bytes", elapsed, n)
	}
}

func procHostile(f *testing.F) {
	for _, s := range []string{"", "null", "{", `{"pid":1e9999}`, `{"pid":"NaN","start":"Inf"}`, "\xff\xfe\x00", strings.Repeat("[", 10001) + strings.Repeat("]", 10001), strings.Repeat("x", 200<<10), `{"pid":4242,"pid":0,"start":"Mon Jan 1 00:00:00 2001"}`} {
		f.Add([]byte(s))
	}
}

const recordedSeed = `{"pid":4242,"start":"Mon Jan 1 00:00:00 2001"}`

func FuzzRecordedSessions(f *testing.F) {
	procHostile(f)
	f.Add([]byte(recordedSeed + "\n" + recordedSeed + "\n"))
	f.Add([]byte("not json\n" + strings.Repeat("x", 200<<10) + "\n" + recordedSeed + "\n"))
	f.Add([]byte(`{"pid":4242,"ppid":1,"pgid":4242,"uid":501,"start":"Mon Jan 1 00:00:00 2001","cmd":"discard me"}`))
	for _, n := range []int{4095, 4096, 4097} {
		f.Add([]byte(recordedSeed + strings.Repeat(" ", n-len(recordedSeed)-1) + "\n" + recordedSeed))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		path := filepath.Join(t.TempDir(), "sessions")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		var got []Proc
		var err error
		procBound(t, len(b), func() { got, err = RecordedSessions(path) })
		if err != nil {
			if len(got) != 0 {
				t.Fatal("read error returned partial sessions")
			}
			t.Fatal(err)
		}
		var want []Proc
		seen := map[Proc]bool{}
		// Model the documented per-line contract, including the newline in the
		// 4096-byte cutoff and accepting a final line without a newline.
		for rest := b; len(rest) > 0; {
			i := bytes.IndexByte(rest, '\n')
			end := len(rest)
			if i >= 0 {
				end = i + 1
			}
			line := rest[:end]
			rest = rest[end:]
			var p Proc
			if len(line) >= 4096 || json.Unmarshal(line, &p) != nil || p.PID <= 1 || p.Start == "" {
				continue
			}
			p = Proc{PID: p.PID, Start: p.Start}
			if !seen[p] {
				seen[p] = true
				want = append(want, p)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("session filtering, order, identity or deduplication mismatch")
		}
		// A read can fail after complete records. Such a result is rejected as
		// a whole; callers must not sweep an incomplete session inventory.
		var failed []Proc
		var readErr error
		procBound(t, len(b), func() { failed, readErr = readSessions(io.MultiReader(bytes.NewReader(b), fuzzReadFailure{})) })
		if readErr == nil || failed != nil {
			t.Fatal("session read failure exposed partial inventory")
		}
		var canonical bytes.Buffer
		for _, p := range got {
			if p.PID <= 1 || p.Start == "" || p != (Proc{PID: p.PID, Start: p.Start}) {
				t.Fatal("invalid session identity")
			}
			json.NewEncoder(&canonical).Encode(p)
		}
		if err := os.WriteFile(path, canonical.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		again, err := RecordedSessions(path)
		if err != nil || !reflect.DeepEqual(got, again) {
			t.Fatal("sessions do not round-trip")
		}
	})
}

type fuzzReadFailure struct{}

func (fuzzReadFailure) Read([]byte) (int, error) { return 0, errors.New("fixture read failure") }

func TestRecordedSessionsDropsPartialOnReadError(t *testing.T) {
	got, err := readSessions(io.MultiReader(strings.NewReader(recordedSeed+"\n"), fuzzReadFailure{}))
	if err == nil || got != nil {
		t.Fatal("read failure returned partial session inventory")
	}
}

func FuzzLiveRegistry(f *testing.F) {
	procHostile(f)
	for _, r := range []Record{
		{Owner: Proc{PID: 100, Start: lookupProc.Start}, Child: lookupProc, Labels: map[string]string{"harness": "fake"}},
		{Owner: lookupProc, Child: Proc{PID: 42421, Start: lookupProc.Start}, Tracked: []Proc{lookupProc}},
	} {
		b, err := json.Marshal(r)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(`{"owner":{"pid":100,"start":"Sun Nov 15 15:45:51 2026"},"tracked":[{"pid":42420}, {"pid":"NaN"}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		path := filepath.Join(t.TempDir(), "live.json")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		var got Record
		var err error
		procBound(t, len(b), func() { got, err = loadRecord(path) })
		unchanged, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(unchanged, b) {
			t.Fatal("registry reader mutated source file")
		}
		var want Record
		decodeErr := json.Unmarshal(b, &want)
		if (err == nil) != (decodeErr == nil) {
			t.Fatal("registry decode status mismatch")
		}
		if err != nil {
			if !reflect.DeepEqual(got, Record{}) {
				t.Fatal("registry error exposed partially decoded processes")
			}
			return
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("registry dropped process identities or labels")
		}
		if err := writeRecord(path, got); err != nil {
			t.Fatal(err)
		}
		again, err := loadRecord(path)
		encoded, _ := json.Marshal(got)
		encodedAgain, _ := json.Marshal(again)
		if err != nil || !bytes.Equal(encoded, encodedAgain) {
			t.Fatal("registry does not round-trip")
		}
	})
}

const psSeed = "  10     1    10   501 Ss   Sun Nov 15 15:45:51 2026 /bin/sh -c sleep  20\n" +
	"  11    10    10   501 Z    Sun Nov 15 15:45:52 2026 (sleep)\n" +
	"  12    10    10   501 SE   Sun Nov 15 15:45:53 2026 (claude)\n"

func FuzzParsePS(f *testing.F) {
	procHostile(f)
	for _, s := range []string{psSeed, psSeed + "garbage\n", "42420 1 42420 501 S not a valid start time helper", "42420 1 42420 501 garbage Sun Nov 15 15:45:51 2026 helper", "0 1 0 501 S Sun Nov 15 15:45:51 2026 helper", "-1 1 0 501 S Sun Nov 15 15:45:51 2026 helper"} {
		f.Add([]byte(s))
	}
	for _, s := range []string{
		psSeed + "10 1 10 501 S Sun Nov 15 15:45:51 2026 replaced command\n",
		"999999999999999999999999999999999 1 10 501 S Sun Nov 15 15:45:51 2026 helper",
		"10 NaN 10 501 S Sun Nov 15 15:45:51 2026 helper",
		"10 1 10 501 S Sun Nov 15 15:45:51 2026 invalid\xff\x00command",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var all map[int]Proc
		var err error
		procBound(t, len(b), func() { all, err = parsePS(string(b)) })
		if err != nil {
			if all != nil {
				t.Fatal("malformed listing exposed partial process map")
			}
			return
		}
		var canonical strings.Builder
		// Iterate source rows rather than the result map to keep replay order
		// deterministic, including repeated PIDs (last live row wins).
		want := map[int]Proc{}
		for _, line := range strings.Split(string(b), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if len(fields) < 11 {
				t.Fatal("accepted short row")
			}
			pid, e := strconv.Atoi(fields[0])
			if e != nil || pid <= 0 {
				t.Fatal("accepted non-positive PID")
			}
			if strings.HasPrefix(fields[4], "Z") || strings.Contains(fields[4], "E") {
				continue
			}
			p := Proc{PID: pid}
			// The final row for a repeated PID is checked after the loop.
			p.PPID, _ = strconv.Atoi(fields[1])
			p.PGID, _ = strconv.Atoi(fields[2])
			p.UID, _ = strconv.Atoi(fields[3])
			p.Start = strings.Join(fields[5:10], " ")
			p.Cmd = strings.Join(fields[10:], " ")
			want[pid] = p
		}
		if !reflect.DeepEqual(all, want) {
			t.Fatal("process identity or duplicate handling mismatch")
		}
		for _, line := range strings.Split(string(b), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			pid, _ := strconv.Atoi(fields[0])
			p, ok := all[pid]
			if !ok {
				continue
			}
			if p.PID <= 0 || p.PID != pid || p.PPID < 0 || p.PGID < 0 || p.UID < 0 {
				t.Fatal("invalid process identifiers")
			}
			if _, ok := startTime(p.Start); !ok {
				t.Fatal("invalid process birth time")
			}
			fmt.Fprintf(&canonical, "%d %d %d %d S %s %s\n", p.PID, p.PPID, p.PGID, p.UID, p.Start, p.Cmd)
		}
		again, replayErr := parsePS(canonical.String())
		if replayErr != nil || !reflect.DeepEqual(all, again) {
			t.Fatal("process listing does not round-trip")
		}
	})
}

func FuzzListPIDOutput(f *testing.F) {
	// The second argument selects an OS result shape without starting a
	// process: success, empty exit 1, diagnostic, other failure, or timeout.
	for _, s := range []string{"", psSeed, "garbage", "42420 1 42420 501 S Sun Nov 15 15:45:51 2026 helper", "42421 1 42421 501 S Sun Nov 15 15:45:51 2026 helper", "42420 1 42420 501 Z Sun Nov 15 15:45:51 2026 (helper)", "42420 1 42420 501 S not a valid start time helper", "42420 1 42420 501 garbage Sun Nov 15 15:45:51 2026 helper", "\xff\x00", strings.Repeat("[", 10001), strings.Repeat("x", 200<<10), `{"pid":"NaN","pid":1e9999}`} {
		for mode := uint8(0); mode < 6; mode++ {
			f.Add([]byte(s), mode)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte, mode uint8) {
		r := psResult{out: b, exitCode: 0}
		failure := errors.New("fixture execution failure")
		switch mode % 6 {
		case 1:
			r.err = failure
			r.exitCode = 1
		case 2:
			r.err = failure
			r.exitCode = 1
			r.stderr = []byte("diagnostic")
		case 3:
			r.err = failure
			r.exitCode = 2
		case 4:
			r.err = failure
			r.exitCode = 1
			r.contextErr = context.DeadlineExceeded
		case 5:
			r.err = failure
			r.exitCode = -1
		}
		var p Proc
		var err error
		procBound(t, len(b), func() { p, err = parsePIDOutput(42420, r) })
		if err != nil && p != (Proc{}) {
			t.Fatal("lookup error returned partial identity")
		}
		if r.err != nil {
			gone := mode%6 == 1 && len(b) == 0
			if err == nil || errors.Is(err, ErrProcessGone) != gone || (!gone && !errors.Is(err, failure)) {
				t.Fatal("failed lookup was mistaken for confirmed absence")
			}
			return
		}
		all, parseErr := parsePS(string(b))
		if parseErr != nil {
			if err == nil || errors.Is(err, ErrProcessGone) {
				t.Fatal("malformed output confirmed absence")
			}
			return
		}
		want, exists := all[42420]
		if !exists {
			if !errors.Is(err, ErrProcessGone) {
				t.Fatal("empty successful lookup was not absent")
			}
			return
		}
		if err != nil || p != want || p.PID <= 0 {
			t.Fatal("lookup lost matching live identity")
		}
	})
}
