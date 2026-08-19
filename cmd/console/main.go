// console: one page showing every fleet node's temperature and the processes actually
// consuming it.
//
// Temperature comes from a running beszel hub (its agent already bundles
// LibreHardwareMonitor and reads Apple Silicon sensors without sudo, so re-collecting
// it here would be a second, worse implementation). Processes come from procd, which
// beszel has no equivalent for.
//
// A node with no procd reachable is rendered as "no procd", never as healthy - an
// untested node silently counted as a pass is how a bogus "all green" gets built.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Node struct {
	Alias  string `json:"alias"`
	NodeID string `json:"node_id"`
	IPv4   string `json:"ipv4"`
	OS     string `json:"os"`
	Role   string `json:"role"`
}

type Proc struct {
	PID    int32   `json:"pid"`
	Name   string  `json:"name"`
	User   string  `json:"user"`
	CPUPct float64 `json:"cpu_pct"`
	RSSMB  float64 `json:"rss_mb"`
	GPUMB  float64 `json:"gpu_mb"`
	OnGPU  bool    `json:"on_gpu"`
	Cmd    string  `json:"cmd"`
}

type procdSnapshot struct {
	Host      string             `json:"host"`
	Taken     time.Time          `json:"taken"`
	CPUPct    float64            `json:"cpu_pct"`
	MemPct    float64            `json:"mem_pct"`
	MemUsedGB float64            `json:"mem_used_gb"`
	Uptime    uint64             `json:"uptime_s"`
	ProcCount int                `json:"proc_count"`
	Temps     map[string]float64 `json:"temps"`
	Procs     []Proc             `json:"procs"`
}

type NodeView struct {
	Node
	ProcdOK   bool               `json:"procd_ok"`
	ProcdErr  string             `json:"procd_err,omitempty"`
	CPUPct    float64            `json:"cpu_pct"`
	MemPct    float64            `json:"mem_pct"`
	MemUsedGB float64            `json:"mem_used_gb"`
	UptimeH   float64            `json:"uptime_h"`
	ProcCount int                `json:"proc_count"`
	Procs     []Proc             `json:"procs"`
	Temps     map[string]float64 `json:"temps"`
	TempMaxC  float64            `json:"temp_max_c"`
	TempMaxAt string             `json:"temp_max_at"`
	TempSrc   string             `json:"temp_source"`
	BeszelAge string             `json:"beszel_age,omitempty"`
	Warnings  []string           `json:"warnings,omitempty"`
}

type Fleet struct {
	Generated time.Time  `json:"generated"`
	Nodes     []NodeView `json:"nodes"`
	Warnings  []string   `json:"warnings"`
}

var (
	rosterPath = flag.String("roster", os.Getenv("KATALA_ROSTER"), "fleet roster TSV (required; or $KATALA_ROSTER). See README for the column layout")
	beszelDB   = flag.String("beszel-db", os.Getenv("KATALA_BESZEL_DB"), "beszel hub SQLite, read-only (optional; or $KATALA_BESZEL_DB). Empty means procd sensors only")
	procdPort  = flag.String("procd-port", "45877", "procd port on each node")
	addr       = flag.String("addr", "127.0.0.1:8770", "listen address")
	refresh    = flag.Duration("refresh", 30*time.Second, "poll interval")
	tempWarnC  = flag.Float64("temp-warn", 84, "warn at or above this temperature")
)

func loadRoster(path string) ([]Node, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Node
	for i, line := range strings.Split(string(raw), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if i == 0 || len(f) < 7 {
			continue
		}
		// retired nodes are never a connection or verification target, and roles marked
		// no-ssh-expected (phones, appliances) carry no agent to poll
		if f[4] != "online" || f[5] == "no-ssh-expected" {
			continue
		}
		out = append(out, Node{Alias: f[0], IPv4: f[2], OS: f[3], Role: f[5], NodeID: f[6]})
	}
	return out, nil
}

// beszelTemps returns node_id -> (sensor -> celsius) from the hub's latest sample, plus
// the sample age, so a stale hub cannot be mistaken for a cool machine.
func beszelTemps(dbPath string) (map[string]map[string]float64, map[string]time.Time, error) {
	temps := map[string]map[string]float64{}
	ages := map[string]time.Time{}
	// immutable+ro: the hub holds this database open in WAL mode and must not be disturbed
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return temps, ages, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT s.name, st.stats, st.created FROM systems s
	  JOIN system_stats st ON st.system = s.id
	  WHERE st.created = (SELECT MAX(created) FROM system_stats WHERE system = s.id)`)
	if err != nil {
		return temps, ages, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, stats, created string
		if err := rows.Scan(&name, &stats, &created); err != nil {
			continue
		}
		var parsed struct {
			T map[string]float64 `json:"t"`
		}
		if json.Unmarshal([]byte(stats), &parsed) == nil && len(parsed.T) > 0 {
			temps[name] = parsed.T
		}
		if ts, err := time.Parse("2006-01-02 15:04:05.999Z", created); err == nil {
			ages[name] = ts
		}
	}
	return temps, ages, rows.Err()
}

// fetchProcd reads a node's snapshot over the existing ssh trust boundary rather than
// over an HTTP port on the private network. That keeps procd bound to loopback
// everywhere, so nothing new is exposed to every peer on the network, and it is the only
// route that reaches nodes whose inbound firewall drops connections to unregistered
// binaries.
func fetchProcd(ctx context.Context, alias, port string, isSelf bool) (*procdSnapshot, error) {
	var raw []byte
	var err error
	if isSelf {
		raw, err = httpGet(ctx, fmt.Sprintf("http://127.0.0.1:%s/procs", port))
	} else {
		raw, err = exec.CommandContext(ctx, "ssh", "-n",
			"-o", "BatchMode=yes", "-o", "ConnectTimeout=8", alias,
			fmt.Sprintf("curl -s -m 8 http://127.0.0.1:%s/procs", port)).Output()
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty response (procd not running?)")
	}
	var snap procdSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("bad payload: %w", err)
	}
	return &snap, nil
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// the console runs on the hub, which reads its own procd directly
var selfNode = strings.ToLower(os.Getenv("KATALA_SELF_NODE"))

var (
	cacheMu sync.RWMutex
	cache   Fleet
)

func collect() Fleet {
	fleet := Fleet{Generated: time.Now()}
	nodes, err := loadRoster(*rosterPath)
	if err != nil {
		fleet.Warnings = append(fleet.Warnings, "roster unreadable: "+err.Error())
		return fleet
	}
	var bTemps map[string]map[string]float64
	var bAges map[string]time.Time
	if *beszelDB != "" {
		if bTemps, bAges, err = beszelTemps(*beszelDB); err != nil {
			fleet.Warnings = append(fleet.Warnings, "beszel temperatures unavailable: "+err.Error())
		}
	}

	views := make([]NodeView, len(nodes))
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n Node) {
			defer wg.Done()
			v := NodeView{Node: n}
			if snap, err := fetchProcd(ctx, n.Alias, *procdPort, n.NodeID == selfNode); err != nil {
				v.ProcdErr = err.Error()
			} else {
				v.ProcdOK = true
				v.CPUPct, v.MemPct, v.MemUsedGB = snap.CPUPct, snap.MemPct, snap.MemUsedGB
				v.UptimeH = float64(snap.Uptime) / 3600
				v.ProcCount, v.Procs = snap.ProcCount, snap.Procs
				v.Temps, v.TempSrc = snap.Temps, "procd"
			}
			// beszel sees more sensors than an unelevated procd, so it wins - but only while
			// fresh. Showing a stale hub reading as the current temperature is the same class
			// of defect as counting an untested node as a pass, and procd stays live even when
			// the hub's agent is down, so a stale sample falls back instead of being displayed.
			if t, ok := bTemps[n.NodeID]; ok {
				age := time.Duration(-1)
				if ts, ok := bAges[n.NodeID]; ok {
					age = time.Since(ts)
					v.BeszelAge = age.Round(time.Second).String()
				}
				switch {
				case age >= 0 && age <= 5*time.Minute:
					if len(t) >= len(v.Temps) {
						v.Temps, v.TempSrc = t, "beszel"
					}
				case len(v.Temps) > 0:
					v.Warnings = append(v.Warnings, fmt.Sprintf(
						"beszel sample is %s old - falling back to procd sensors (hub agent down?)", v.BeszelAge))
				default:
					v.Warnings = append(v.Warnings, fmt.Sprintf(
						"beszel sample is %s old and procd has no sensor - temperature unknown", v.BeszelAge))
				}
			}
			for k, c := range v.Temps {
				if c > v.TempMaxC {
					v.TempMaxC, v.TempMaxAt = c, k
				}
			}
			if v.TempMaxC >= *tempWarnC {
				v.Warnings = append(v.Warnings,
					fmt.Sprintf("%s at %.0fC (>= %.0f)", v.TempMaxAt, v.TempMaxC, *tempWarnC))
			}
			if len(v.Temps) == 0 {
				v.Warnings = append(v.Warnings, "no temperature sensor readable (Windows CPU needs beszel agent with LHM=true and admin)")
			}
			if !v.ProcdOK {
				v.Warnings = append(v.Warnings, "no procd: "+v.ProcdErr)
			}
			views[i] = v
		}(i, n)
	}
	wg.Wait()

	sort.Slice(views, func(i, j int) bool { return views[i].TempMaxC > views[j].TempMaxC })
	fleet.Nodes = views
	return fleet
}

var page = template.Must(template.New("p").Funcs(template.FuncMap{
	"pct": func(f float64) string { return fmt.Sprintf("%.0f%%", f) },
}).Parse(`<!doctype html><meta charset=utf-8><title>Katala Fleet Console</title>
<meta name=viewport content="width=device-width,initial-scale=1">
<style>
:root{color-scheme:dark light}
body{font:13px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;margin:0;padding:12px;background:#111;color:#ddd}
h1{font-size:15px;margin:0 0 10px;font-weight:600}
.grid{display:grid;gap:10px;grid-template-columns:repeat(auto-fit,minmax(340px,1fr))}
.card{border:1px solid #333;border-radius:6px;padding:10px;background:#181818}
.hd{display:flex;justify-content:space-between;align-items:baseline;gap:8px}
.name{font-weight:600}
.t{font-size:20px;font-weight:600}
.warm{color:#e8a33d}.hot{color:#e05561}.cool{color:#5eb0ef}.dead{color:#777}
.meta{color:#888;font-size:11px;margin:2px 0 6px}
table{width:100%;border-collapse:collapse}
td{padding:1px 3px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
td.n{max-width:150px}td.r{text-align:right;color:#aaa}
.w{color:#e8a33d;font-size:11px;margin-top:5px}
.s{color:#666;font-size:11px}
</style>
<h1>Katala Fleet Console <span class=s>{{.Generated.Format "2006-01-02 15:04:05"}} / auto-refresh</span></h1>
{{range .Warnings}}<div class=w>! {{.}}</div>{{end}}
<div class=grid>
{{range .Nodes}}<div class=card>
 <div class=hd>
  <span class=name>{{.Alias}}</span>
  <span class="t {{if ge .TempMaxC 84.0}}hot{{else if ge .TempMaxC 70.0}}warm{{else if gt .TempMaxC 0.0}}cool{{else}}dead{{end}}">{{if gt .TempMaxC 0.0}}{{printf "%.0f" .TempMaxC}}&deg;C{{else}}--{{end}}</span>
 </div>
 <div class=meta>{{.Role}} / {{.OS}} / {{.IPv4}}{{if gt .TempMaxC 0.0}} &middot; {{.TempMaxAt}} ({{.TempSrc}}){{end}}</div>
 {{if .ProcdOK}}<div class=meta>cpu {{pct .CPUPct}} &middot; mem {{pct .MemPct}} ({{printf "%.1f" .MemUsedGB}}GB) &middot; {{.ProcCount}} procs &middot; up {{printf "%.0f" .UptimeH}}h</div>
 <table>{{range .Procs}}<tr><td class=n title="{{.Cmd}}">{{.Name}}</td><td class=r>{{printf "%.1f" .CPUPct}}%</td><td class=r>{{printf "%.0f" .RSSMB}}M</td><td class=r>{{if .OnGPU}}{{if gt .GPUMB 0.0}}gpu {{printf "%.0f" .GPUMB}}M{{else}}gpu{{end}}{{end}}</td></tr>{{end}}</table>
 {{else}}<div class=meta>process list unavailable</div>{{end}}
 {{range .Warnings}}<div class=w>! {{.}}</div>{{end}}
</div>{{end}}
</div>
<script>setTimeout(()=>location.reload(),15000)</script>`))

func main() {
	flag.Parse()
	if *rosterPath == "" {
		log.Fatal("-roster is required (or set $KATALA_ROSTER); see README for the TSV column layout")
	}
	cache = collect()
	go func() {
		for range time.Tick(*refresh) {
			f := collect()
			cacheMu.Lock()
			cache = f
			cacheMu.Unlock()
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		cacheMu.RLock()
		f := cache
		cacheMu.RUnlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, f); err != nil {
			log.Println("render:", err)
		}
	})
	http.HandleFunc("/api/fleet", func(w http.ResponseWriter, r *http.Request) {
		cacheMu.RLock()
		f := cache
		cacheMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", " ")
		enc.Encode(f)
	})

	log.Printf("console on http://%s (nodes=%d)", *addr, len(cache.Nodes))
	srv := &http.Server{Addr: *addr, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
