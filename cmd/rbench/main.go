// katala-rbench: measures a node's real remote-desktop capability on Windows, macOS and
// Linux, then reports it as JSON.
//
// Why measure instead of read a table: `ffmpeg -encoders` has been observed listing av1_nvenc on
// an an Ampere-generation GeForce (Ampere cannot encode AV1) and listed h264_nvenc on an a Blackwell-generation GeForce whose
// driver was too old to open it. Both would have been recorded as "supported" by any check
// that trusts the encoder list. Only an actual encode settles it, so every codec here is
// exercised on a real frame sequence and timed.
//
// The timing doubles as the throughput number that matters for a remote desktop: encoding
// 1080p60 in less than a second of wall clock is the floor for a 60 fps session.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type EncoderResult struct {
	Codec      string  `json:"codec"`
	Available  bool    `json:"available"`
	Kind       string  `json:"kind"` // hardware | software
	EncodeFPS  float64 `json:"encode_fps,omitempty"`
	RealtimeOK bool    `json:"realtime_ok"` // clears 60 fps with headroom
	Error      string  `json:"error,omitempty"`
}

type GPU struct {
	Name   string `json:"name"`
	Driver string `json:"driver,omitempty"`
}

// DecoderResult is the receiving half of a session. It is separate from the encoder result
// because a node is often able to encode a codec it cannot hardware-decode, and vice versa
// (Ampere decodes AV1 but cannot encode it; M2 does neither while M3 decodes it).
type DecoderResult struct {
	Codec   string `json:"codec"`
	HWAccel string `json:"hw_accel,omitempty"` // the accel that worked
	HW      bool   `json:"hw"`
	SW      bool   `json:"sw"`
	Error   string `json:"error,omitempty"`
}

type Report struct {
	Host       string          `json:"host"`
	OS         string          `json:"os"`
	Arch       string          `json:"arch"`
	Taken      time.Time       `json:"taken"`
	FFmpeg     string          `json:"ffmpeg"`
	GPUs       []GPU           `json:"gpus"`
	Encoders   []EncoderResult `json:"encoders"`
	Decoders   []DecoderResult `json:"decoders"`
	BestCodec  string          `json:"best_codec"`
	RemoteApps []string        `json:"remote_apps"`
	Notes      []string        `json:"notes"`
}

var (
	asJSON  = flag.Bool("json", false, "emit JSON only")
	frames  = flag.Int("frames", 300, "frames to encode per codec (5s of 60fps; short runs are dominated by encoder session setup)")
	width   = flag.Int("width", 1920, "test frame width")
	height  = flag.Int("height", 1080, "test frame height")
	timeout = flag.Duration("timeout", 25*time.Second, "per-codec timeout")
	skipDec = flag.Bool("skip-decode", false, "skip the decode probe (it writes small temp files)")
)

// hwAccels are the decode paths worth trying per OS. -hwaccel_output_format is set with
// them so ffmpeg cannot silently fall back to software: without it a failed hardware decode
// still exits 0, and the result would read as "hardware decode works" - the same class of
// false positive as trusting the encoder list.
func hwAccels() map[string]string {
	switch runtime.GOOS {
	case "windows":
		return map[string]string{"cuda": "cuda", "qsv": "qsv", "d3d11va": "d3d11"}
	case "darwin":
		return map[string]string{"videotoolbox": "videotoolbox_vld"}
	default:
		return map[string]string{"cuda": "cuda", "vaapi": "vaapi", "qsv": "qsv"}
	}
}

// softEncoderFor names the library encoder used to mint a decode sample. These are separate
// from the hardware encoders: a node that cannot ENCODE av1 can still be asked to DECODE it,
// so the sample has to come from software.
func softEncoderFor(codec string) []string {
	switch codec {
	case "h264":
		return []string{"libx264"}
	case "hevc":
		return []string{"libx265"}
	case "av1":
		return []string{"libsvtav1", "libaom-av1"}
	}
	return nil
}

// candidates lists the hardware paths worth testing per OS plus one software baseline, so
// the hardware gain is a measured ratio rather than an assumption.
func candidates() []EncoderResult {
	var c []EncoderResult
	hw := func(name string) { c = append(c, EncoderResult{Codec: name, Kind: "hardware"}) }
	switch runtime.GOOS {
	case "windows":
		for _, n := range []string{
			"h264_nvenc", "hevc_nvenc", "av1_nvenc",
			"h264_amf", "hevc_amf", "av1_amf",
			"h264_qsv", "hevc_qsv", "av1_qsv",
		} {
			hw(n)
		}
	case "darwin":
		for _, n := range []string{"h264_videotoolbox", "hevc_videotoolbox"} {
			hw(n)
		}
	default: // linux, incl. WSL
		for _, n := range []string{
			"h264_nvenc", "hevc_nvenc", "av1_nvenc",
			"h264_vaapi", "hevc_vaapi", "av1_vaapi",
			"h264_qsv", "hevc_qsv",
		} {
			hw(n)
		}
	}
	c = append(c, EncoderResult{Codec: "libx264", Kind: "software"})
	return c
}

// codecRank orders by bitrate efficiency at equal quality: AV1 beats HEVC beats H.264.
// Lower bitrate at equal quality is lower latency on a constrained link, which is why the
// pairing matrix prefers the newest codec both ends can actually handle.
//
// Matching on a substring rather than a prefix: software encoders are named after their
// library, not the codec (libx264 / libx265 / libsvtav1), so a prefix test scored every one
// of them 0 and left the ranking unable to compare software options at all.
func codecRank(codec string) int {
	switch {
	case strings.Contains(codec, "av1"):
		return 3
	case strings.Contains(codec, "hevc"), strings.Contains(codec, "x265"):
		return 2
	case strings.Contains(codec, "h264"), strings.Contains(codec, "x264"):
		return 1
	}
	return 0
}

func ffmpegPath() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

// testEncode runs one real encode and returns achieved fps. A codec that is listed but
// cannot open (wrong GPU generation, driver too old) fails here, which is the point.
func testEncode(ff, codec string, n, w, h int, to time.Duration) (float64, error) {
	// testsrc2 moves and has detail. A flat colour source compresses so easily that
	// libx264 outruns NVENC on it, which inverts the ranking a remote desktop actually
	// sees - the measurement has to look like a real screen, not a best case.
	src := fmt.Sprintf("testsrc2=s=%dx%d:r=60:d=%.3f", w, h, float64(n)/60.0)
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", src,
		"-c:v", codec,
	}
	// software baseline runs the settings a remote desktop would actually use; comparing
	// against libx264's quality-first default would flatter the hardware for the wrong reason
	if codec == "libx264" {
		args = append(args, "-preset", "ultrafast", "-tune", "zerolatency")
	}
	args = append(args, "-f", "null", "-")
	cmd := exec.Command(ff, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	start := time.Now()
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		elapsed := time.Since(start).Seconds()
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if i := strings.Index(msg, "\n"); i > 0 {
				msg = msg[:i]
			}
			if msg == "" {
				msg = err.Error()
			}
			return 0, fmt.Errorf("%s", msg)
		}
		if elapsed <= 0 {
			return 0, fmt.Errorf("zero elapsed time")
		}
		return float64(n) / elapsed, nil
	case <-time.After(to):
		_ = cmd.Process.Kill()
		return 0, fmt.Errorf("timeout after %s", to)
	}
}

// probeDecoders mints a short sample per codec with a software encoder, then tries every
// hardware decode path. A codec whose sample cannot be produced is reported as unmeasured
// rather than unsupported - the distinction is the whole point of this tool.
func probeDecoders(ff string, to time.Duration) ([]DecoderResult, []string) {
	var out []DecoderResult
	var notes []string
	dir, err := os.MkdirTemp("", "rbench")
	if err != nil {
		return nil, []string{"decode probe skipped: " + err.Error()}
	}
	defer os.RemoveAll(dir)

	accels := hwAccels()
	for _, codec := range []string{"h264", "hevc", "av1"} {
		sample := filepath.Join(dir, codec+".mp4")
		made := false
		for _, enc := range softEncoderFor(codec) {
			args := []string{"-hide_banner", "-loglevel", "error", "-y",
				"-f", "lavfi", "-i", "testsrc2=s=640x480:r=30:d=1",
				"-c:v", enc}
			if enc == "libsvtav1" {
				args = append(args, "-preset", "12") // fastest; this is a fixture, not a benchmark
			}
			args = append(args, sample)
			cmd := exec.Command(ff, args...)
			if err := runWithTimeout(cmd, to); err == nil {
				made = true
				break
			}
		}
		if !made {
			out = append(out, DecoderResult{Codec: codec,
				Error: "no software encoder available to mint a sample: capability NOT MEASURED"})
			continue
		}

		res := DecoderResult{Codec: codec}
		// software decode is the floor; if even this fails the sample itself is suspect
		swCmd := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-i", sample, "-f", "null", "-")
		res.SW = runWithTimeout(swCmd, to) == nil

		for accel, outFmt := range accels {
			cmd := exec.Command(ff, "-hide_banner", "-loglevel", "error",
				"-hwaccel", accel, "-hwaccel_output_format", outFmt,
				"-i", sample, "-f", "null", "-")
			if runWithTimeout(cmd, to) == nil {
				res.HW = true
				res.HWAccel = accel
				break
			}
		}
		if !res.HW {
			res.Error = "no hardware decode path succeeded (" + strings.Join(accelNames(accels), "/") + ")"
		}
		out = append(out, res)
	}
	return out, notes
}

func accelNames(m map[string]string) []string {
	var n []string
	for k := range m {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func runWithTimeout(cmd *exec.Cmd, to time.Duration) error {
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(to):
		_ = cmd.Process.Kill()
		return fmt.Errorf("timeout")
	}
}

func detectGPUs() []GPU {
	var out []GPU
	if raw, err := exec.Command("nvidia-smi",
		"--query-gpu=name,driver_version", "--format=csv,noheader").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			f := strings.SplitN(line, ",", 2)
			if len(f) == 2 {
				out = append(out, GPU{Name: strings.TrimSpace(f[0]), Driver: strings.TrimSpace(f[1])})
			}
		}
	}
	if runtime.GOOS == "darwin" && len(out) == 0 {
		// Apple Silicon has no separate driver version; the chip name is the capability
		if raw, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			out = append(out, GPU{Name: strings.TrimSpace(string(raw)) + " (integrated)"})
		}
	}
	return out
}

// detectRemoteApps looks for the remote-desktop stacks already installed, so a new one is
// never introduced where a working path exists.
func detectRemoteApps() []string {
	names := map[string][]string{
		"parsec":     {"parsecd", "parsecd.exe"},
		"rustdesk":   {"rustdesk", "rustdesk.exe"},
		"anydesk":    {"AnyDesk", "AnyDesk.exe"},
		"sunshine":   {"sunshine", "sunshine.exe"},
		"moonlight":  {"moonlight", "Moonlight.exe"},
		"teamviewer": {"TeamViewer", "TeamViewer.exe"},
	}
	var running string
	switch runtime.GOOS {
	case "windows":
		if raw, err := exec.Command("tasklist", "/fo", "csv", "/nh").Output(); err == nil {
			running = strings.ToLower(string(raw))
		}
	default:
		if raw, err := exec.Command("ps", "ax").Output(); err == nil {
			running = strings.ToLower(string(raw))
		}
	}
	var found []string
	for app, procs := range names {
		for _, p := range procs {
			if strings.Contains(running, strings.ToLower(p)) {
				found = append(found, app)
				break
			}
		}
	}
	sort.Strings(found)
	return found
}

func main() {
	flag.Parse()
	host, _ := os.Hostname()
	rep := Report{
		Host: host, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Taken: time.Now(), GPUs: detectGPUs(), RemoteApps: detectRemoteApps(),
	}
	rep.FFmpeg = ffmpegPath()
	rep.Notes = append(rep.Notes, fmt.Sprintf(
		"measured with testsrc2 %dx%d, %d frames; software baseline uses -preset ultrafast -tune zerolatency",
		*width, *height, *frames))
	// fps answers "does this codec keep up", not "is it the better choice". A software
	// encoder can post a higher fps on a synthetic pattern while still being the wrong
	// pick: it burns CPU the session shares with games and training, and at equal quality
	// it needs more bitrate. Hardware therefore wins best_codec whenever it is available.
	rep.Notes = append(rep.Notes,
		"encode_fps is a realtime-headroom figure only; hardware vs software is decided by CPU cost and bitrate efficiency, which fps cannot show")

	if rep.FFmpeg == "" {
		// not measured is not the same as not capable; say so instead of reporting zeros
		rep.Notes = append(rep.Notes,
			"ffmpeg not found: encoder capability NOT MEASURED (install ffmpeg to measure)")
		rep.Encoders = nil
	} else {
		for _, c := range candidates() {
			fps, err := testEncode(rep.FFmpeg, c.Codec, *frames, *width, *height, *timeout)
			if err != nil {
				c.Available = false
				c.Error = err.Error()
			} else {
				c.Available = true
				c.EncodeFPS = float64(int(fps*10+0.5)) / 10
				// 90 fps on a 60 fps target leaves room for the capture and network stages
				c.RealtimeOK = c.EncodeFPS >= 90
			}
			rep.Encoders = append(rep.Encoders, c)
		}
		// best = highest-ranked codec that actually encoded, hardware preferred
		bestRank, bestHW := -1, false
		for _, e := range rep.Encoders {
			if !e.Available {
				continue
			}
			r := codecRank(e.Codec)
			isHW := e.Kind == "hardware"
			if isHW != bestHW {
				if isHW {
					rep.BestCodec, bestRank, bestHW = e.Codec, r, true
				}
				continue
			}
			if r > bestRank {
				rep.BestCodec, bestRank = e.Codec, r
			}
		}
		if rep.BestCodec == "" {
			rep.Notes = append(rep.Notes, "no encoder opened successfully - remote sessions fall back to software encoding")
		}
		if !*skipDec {
			dec, dnotes := probeDecoders(rep.FFmpeg, *timeout)
			rep.Decoders = dec
			rep.Notes = append(rep.Notes, dnotes...)
		} else {
			rep.Notes = append(rep.Notes, "decode capability NOT MEASURED (-skip-decode)")
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		_ = enc.Encode(rep)
		return
	}
	fmt.Printf("%s (%s/%s) ffmpeg=%s\n", rep.Host, rep.OS, rep.Arch,
		map[bool]string{true: rep.FFmpeg, false: "MISSING"}[rep.FFmpeg != ""])
	for _, g := range rep.GPUs {
		fmt.Printf("  gpu: %s driver=%s\n", g.Name, g.Driver)
	}
	for _, e := range rep.Encoders {
		if e.Available {
			fmt.Printf("  %-20s %-8s %6.1f fps\n", e.Codec, e.Kind, e.EncodeFPS)
		} else {
			fmt.Printf("  %-20s %-8s FAIL: %s\n", e.Codec, e.Kind, e.Error)
		}
	}
	for _, d := range rep.Decoders {
		switch {
		case d.HW:
			fmt.Printf("  decode %-5s hw via %s\n", d.Codec, d.HWAccel)
		case d.SW:
			fmt.Printf("  decode %-5s SOFTWARE ONLY: %s\n", d.Codec, d.Error)
		default:
			fmt.Printf("  decode %-5s NOT MEASURED: %s\n", d.Codec, d.Error)
		}
	}
	fmt.Printf("  best: %s / apps: %s\n", rep.BestCodec, strings.Join(rep.RemoteApps, ","))
	for _, n := range rep.Notes {
		fmt.Printf("  ! %s\n", n)
	}
}
