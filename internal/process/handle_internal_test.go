package process

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestParseProcStat covers the format's one genuine trap: comm is the
// executable's name in parentheses, and it may contain spaces and parentheses
// of its own. A binary called "my program (beta)" is unusual and perfectly
// legal, and a parser that splits the line on spaces reads the wrong field for
// every process on the host — including, one day, a container's.
func TestParseProcStat(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantState byte
		wantStart uint64
		wantErr   bool
	}{
		{
			name:      "ordinary",
			line:      "4242 (forge-init) S 1 4242 4242 0 -1 4194560 " + filler(12) + " 9948213 rest",
			wantState: 'S',
			wantStart: 9948213,
		},
		{
			name:      "comm with spaces and parentheses",
			line:      "4242 (my program (beta)) R 1 4242 4242 0 -1 4194560 " + filler(12) + " 77 rest",
			wantState: 'R',
			wantStart: 77,
		},
		{
			name:      "zombie",
			line:      "4242 (forge-init) Z 1 4242 4242 0 -1 4194560 " + filler(12) + " 5 rest",
			wantState: 'Z',
			wantStart: 5,
		},
		{
			name:      "comm ending in a digit",
			line:      "4242 (sh) D 1 4242 4242 0 -1 4194560 " + filler(12) + " 123456789 rest",
			wantState: 'D',
			wantStart: 123456789,
		},
		{name: "no comm", line: "4242 forge-init S 1 2 3", wantErr: true},
		{name: "truncated", line: "4242 (forge-init) S 1 2 3", wantErr: true},
		{name: "empty", line: "", wantErr: true},
		{
			name:    "unparseable start time",
			line:    "4242 (forge-init) S 1 4242 4242 0 -1 4194560 " + filler(12) + " later rest",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProcStat(tc.line, 4242)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseProcStat() = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProcStat() = %v", err)
			}
			if got.state != tc.wantState {
				t.Errorf("state = %q, want %q", got.state, tc.wantState)
			}
			if got.startTicks != tc.wantStart {
				t.Errorf("startTicks = %d, want %d", got.startTicks, tc.wantStart)
			}
		})
	}
}

// TestParseProcStatAgainstTheRealThing checks the field arithmetic against a
// line the kernel actually wrote, which is the only way to be sure the count
// is right rather than merely self-consistent.
func TestParseProcStatAgainstTheRealThing(t *testing.T) {
	pid := os.Getpid()

	got, err := readProcStat(pid)
	if err != nil {
		t.Fatalf("readProcStat(%d) = %v", pid, err)
	}

	// This process is running, and nothing that is running has a start time of
	// zero — the boot itself is tick zero and nothing that reads its own stat
	// started then.
	if got.state != 'R' && got.state != 'S' {
		t.Errorf("state = %q, want R or S for the running test binary", got.state)
	}
	if got.startTicks == 0 {
		t.Error("startTicks = 0, want the test binary's real start time")
	}

	// An independent bound on the field arithmetic. A process cannot have
	// started before the host booted, so its start time in ticks is below the
	// uptime in ticks — and the fields on either side of the right one fail
	// this badly: field 21 is always 0 for a normal process, and field 23 is
	// the address-space size, which is orders of magnitude too large. The
	// 1000 is a generous ceiling on CLK_TCK, which is 100 everywhere in
	// practice.
	uptime, err := os.ReadFile("/proc/uptime")
	if err != nil {
		t.Skipf("no /proc/uptime to check against: %v", err)
	}
	seconds, err := strconv.ParseFloat(strings.Fields(string(uptime))[0], 64)
	if err != nil {
		t.Fatalf("parsing /proc/uptime: %v", err)
	}
	if ceiling := uint64(seconds * 1000); got.startTicks >= ceiling {
		t.Errorf("startTicks = %d, want below %d (uptime): the field offset is wrong",
			got.startTicks, ceiling)
	}
}

func TestReadProcStatReportsAMissingProcess(t *testing.T) {
	// PID 0 has no /proc entry: it is the scheduler's placeholder, never a
	// process a caller can name.
	if _, err := readProcStat(0); err == nil {
		t.Fatal("readProcStat(0) = nil error, want a failure")
	}
}

// filler returns n placeholder fields, standing in for the parts of
// /proc/<pid>/stat between the flags and the start time that this package does
// not read.
func filler(n int) string {
	return strings.TrimSpace(strings.Repeat("0 ", n))
}
