// procd: per-node process/temperature sidecar.
//
// Read-only by design: it exposes what is running and how hot the box is, and has no
// endpoint that changes node state. beszel already covers host metrics and sensors;
// this fills the gap beszel has no concept of - which PROCESS is eating the machine.
//
// Bind stays on loopback. The console reads it over ssh, so the ssh key is the only
// access boundary and no second auth scheme is invented. Binding -addr to a routable
// interface exposes an unauthenticated endpoint - do that only behind your own network
// boundary.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
	gosensors "github.com/shirou/gopsutil/v4/sensors"
)

type Proc struct {
	PID     int32   `json:"pid"`
	Name    string  `json:"name"`
	User    string  `json:"user"`
	CPUPct  float64 `json:"cpu_pct"`
	RSSMB   float64 `json:"rss_mb"`
	GPUMB   float64 `json:"gpu_mb,omitempty"`
	OnGPU   bool    `json:"on_gpu,omitempty"`
	Cmdline string  `json:"cmd,omitempty"`
}

type Snapshot struct {
	Host      string             `json:"host"`
	OS        string             `json:"os"`
	Taken     time.Time          `json:"taken"`
	CPUPct    float64            `json:"cpu_pct"`
	MemPct    float64            `json:"mem_pct"`
	MemUsedGB float64            `json:"mem_used_gb"`
	Load1     float64            `json:"load1"`
	Uptime    uint64             `json:"uptime_s"`
	ProcCount int                `json:"proc_count"`
	Temps     map[string]float64 `json:"temps,omitempty"`
	Procs     []Proc             `json:"procs"`
}

var (
	mu      sync.RWMutex
	current Snapshot
	topN    = flag.Int("top", 20, "processes to report, ranked by CPU")
	addr    = flag.String("addr", "127.0.0.1:45877", "listen address")
	every   = flag.Duration("interval", 4*time.Second, "sampling interval")
)

// gopsutil reports per-process CPU as an average since process start unless the same
// Process object is polled twice, so the previous CPU time is kept per PID and the
// percentage is derived from the delta over the real elapsed interval.
type cpuTimeEntry struct {
	total float64
	seen  time.Time
}

var lastCPU = map[int32]cpuTimeEntry{}

func sampleProcs(elapsed time.Duration) ([]Proc, int) {
	procs, err := process.Processes()
	if err != nil {
		return nil, 0
	}
	cores := float64(runtime.NumCPU())
	gpu := gpuMemByPID()
	seen := make(map[int32]bool, len(procs))
	out := make([]Proc, 0, len(procs))

	for _, p := range procs {
		t, err := p.Times()
		if err != nil {
			continue
		}
		total := t.User + t.System
		seen[p.Pid] = true
		prev, ok := lastCPU[p.Pid]
		lastCPU[p.Pid] = cpuTimeEntry{total: total, seen: time.Now()}
		if !ok || elapsed <= 0 {
			continue // first sighting has no delta to report
		}
		pct := (total - prev.total) / elapsed.Seconds() * 100 / cores
		if pct < 0 {
			pct = 0
		}
		name, _ := p.Name()
		user, _ := p.Username()
		var rss float64
		if mi, err := p.MemoryInfo(); err == nil && mi != nil {
			rss = float64(mi.RSS) / 1024 / 1024
		}
		_, onGPU := gpu[p.Pid]
		cmd, _ := p.Cmdline()
		if len(cmd) > 160 {
			cmd = cmd[:160]
		}
		out = append(out, Proc{
			PID: p.Pid, Name: name, User: shortUser(user),
			CPUPct: round(pct, 1), RSSMB: round(rss, 1),
			GPUMB: gpu[p.Pid], OnGPU: onGPU, Cmdline: cmd,
		})
	}
	for pid := range lastCPU {
		if !seen[pid] {
			delete(lastCPU, pid) // otherwise the map grows for the process lifetime
		}
	}
	// GPU tenants matter even when idle on CPU, so they outrank plain CPU order
	sort.Slice(out, func(i, j int) bool {
		if out[i].OnGPU != out[j].OnGPU {
			return out[i].OnGPU
		}
		return out[i].CPUPct > out[j].CPUPct
	})
	count := len(procs)
	if len(out) > *topN {
		out = out[:*topN]
	}
	return out, count
}

func shortUser(u string) string {
	if i := strings.LastIndexAny(u, `\/`); i >= 0 {
		return u[i+1:]
	}
	return u
}

// gpuMemByPID returns the PIDs holding the GPU, mapped to their VRAM use in MiB.
// On Windows the value is almost always absent: nvidia-smi reports per-process memory
// as [N/A] under WDDM because the OS owns VRAM allocation, and GeForce cards cannot be
// switched to TCC. The PID list is still accurate, so membership means "on the GPU"
// even when the size is 0 - callers must not read 0 as "not using the GPU".
func gpuMemByPID() map[int32]float64 {
	out := map[int32]float64{}
	raw, err := exec.Command("nvidia-smi",
		"--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Split(line, ",")
		if len(f) != 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		mb, err := strconv.ParseFloat(strings.TrimSpace(f[1]), 64)
		if err != nil {
			mb = 0 // [N/A] under WDDM; the PID is still a real GPU tenant
		}
		out[int32(pid)] = mb
	}
	return out
}

// temps reads sensors where they are readable without elevation. On Windows this is
// usually only the ACPI thermal zone plus the GPU; beszel's bundled LibreHardwareMonitor
// covers the rest and this endpoint does not try to duplicate it.
func temps() map[string]float64 {
	out := map[string]float64{}
	if ts, err := sensors(); err == nil {
		for k, v := range ts {
			out[k] = v
		}
	}
	if raw, err := exec.Command("nvidia-smi",
		"--query-gpu=name,temperature.gpu", "--format=csv,noheader,nounits").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			f := strings.SplitN(line, ",", 2)
			if len(f) == 2 {
				if c, err := strconv.ParseFloat(strings.TrimSpace(f[1]), 64); err == nil {
					out[strings.TrimSpace(f[0])] = c
				}
			}
		}
	}
	return out
}

func sensors() (map[string]float64, error) {
	list, err := gosensors.SensorsTemperatures()
	if err != nil && len(list) == 0 {
		return nil, err
	}
	out := make(map[string]float64, len(list))
	for _, s := range list {
		if s.Temperature > 0 {
			out[s.SensorKey] = round(s.Temperature, 2)
		}
	}
	return out, nil
}

func sample() {
	start := time.Now()
	prev := current.Taken
	elapsed := start.Sub(prev)
	if prev.IsZero() {
		elapsed = 0
	}
	procs, count := sampleProcs(elapsed)

	snap := Snapshot{OS: runtime.GOOS, Taken: start, ProcCount: count, Procs: procs}
	if h, err := host.Info(); err == nil {
		snap.Host = h.Hostname
		snap.Uptime = h.Uptime
	}
	if c, err := cpu.Percent(0, false); err == nil && len(c) > 0 {
		snap.CPUPct = round(c[0], 1)
	}
	if m, err := mem.VirtualMemory(); err == nil {
		snap.MemPct = round(m.UsedPercent, 1)
		snap.MemUsedGB = round(float64(m.Used)/1e9, 1)
	}
	if l, err := load.Avg(); err == nil {
		snap.Load1 = round(l.Load1, 2)
	}
	snap.Temps = temps()

	mu.Lock()
	current = snap
	mu.Unlock()
}

func round(f float64, places int) float64 {
	p := 1.0
	for i := 0; i < places; i++ {
		p *= 10
	}
	return float64(int64(f*p+0.5)) / p
}

func main() {
	flag.Parse()
	sample() // seed the CPU deltas so the first request is not empty
	go func() {
		for range time.Tick(*every) {
			sample()
		}
	}()

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		age := time.Since(current.Taken).Seconds()
		mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": age < 60, "sample_age_s": round(age, 1)})
	})
	http.HandleFunc("/procs", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		snap := current
		mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", " ")
		enc.Encode(snap)
	})

	log.Printf("katala-procd on %s (top=%d interval=%s os=%s)", *addr, *topN, *every, runtime.GOOS)
	srv := &http.Server{Addr: *addr, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}
