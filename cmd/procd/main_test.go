package main

import "testing"

// CPU percent comes from a time delta, not from gopsutil's since-boot average, so a
// first sighting must report nothing rather than a made-up number.
func TestCPUDeltaNeedsTwoSamples(t *testing.T) {
	lastCPU = map[int32]cpuTimeEntry{}
	procs, count := sampleProcs(0)
	if count == 0 {
		t.Skip("no processes visible in this environment")
	}
	if len(procs) != 0 {
		t.Fatalf("first sample must yield no percentages, got %d rows", len(procs))
	}
	if len(lastCPU) == 0 {
		t.Fatal("first sample must still record CPU times for the next delta")
	}
	// second pass over a real interval must produce rows
	procs2, _ := sampleProcs(2_000_000_000)
	if len(procs2) == 0 {
		t.Fatal("second sample produced no rows")
	}
	for _, p := range procs2 {
		if p.CPUPct < 0 {
			t.Fatalf("negative cpu for pid %d: %v", p.PID, p.CPUPct)
		}
		if p.CPUPct > 100 {
			t.Fatalf("cpu %v exceeds 100%% for pid %d - not normalised by core count", p.CPUPct, p.PID)
		}
	}
}

func TestShortUser(t *testing.T) {
	for in, want := range map[string]string{
		`WINHOST\alice`: "alice",
		"/Users/alice":  "alice",
		"alice":         "alice",
		"":              "",
	} {
		if got := shortUser(in); got != want {
			t.Errorf("shortUser(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRound(t *testing.T) {
	if got := round(12.345, 1); got != 12.3 {
		t.Errorf("round(12.345,1) = %v", got)
	}
	if got := round(0.0, 2); got != 0 {
		t.Errorf("round(0,2) = %v", got)
	}
}
