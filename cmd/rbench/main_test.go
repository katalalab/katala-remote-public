package main

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// AV1 must outrank HEVC must outrank H.264: the ranking drives which codec a session is
// told to prefer, and getting it backwards would pick the most bandwidth-hungry option.
func TestCodecRank(t *testing.T) {
	if !(codecRank("av1_nvenc") > codecRank("hevc_nvenc")) {
		t.Error("av1 must outrank hevc")
	}
	if !(codecRank("hevc_videotoolbox") > codecRank("h264_videotoolbox")) {
		t.Error("hevc must outrank h264")
	}
	if codecRank("libx264") != 1 {
		t.Errorf("libx264 is an h264 encoder: rank=%d", codecRank("libx264"))
	}
	if codecRank("something_else") != 0 {
		t.Error("unknown codec must rank 0, not accidentally beat a real one")
	}
}

// The candidate list must be OS-appropriate and must always carry a software baseline,
// otherwise a node with no working hardware encoder reports nothing at all instead of
// "software only".
func TestCandidatesPerOS(t *testing.T) {
	c := candidates()
	if len(c) < 2 {
		t.Fatalf("too few candidates: %d", len(c))
	}
	var sw, hw int
	for _, e := range c {
		switch e.Kind {
		case "software":
			sw++
		case "hardware":
			hw++
		default:
			t.Errorf("candidate %q has unknown kind %q", e.Codec, e.Kind)
		}
		if e.Available {
			t.Errorf("candidate %q must start unmeasured, not available", e.Codec)
		}
	}
	if sw != 1 {
		t.Errorf("want exactly one software baseline, got %d", sw)
	}
	if hw == 0 {
		t.Error("no hardware candidates for this OS")
	}
	joined := strings.Join(func() []string {
		var n []string
		for _, e := range c {
			n = append(n, e.Codec)
		}
		return n
	}(), ",")
	switch runtime.GOOS {
	case "darwin":
		if !strings.Contains(joined, "videotoolbox") {
			t.Error("darwin must test VideoToolbox")
		}
		if strings.Contains(joined, "nvenc") {
			t.Error("darwin must not test NVENC")
		}
	case "windows":
		// all three vendors matter: an a Blackwell-generation GeForce with unusable NVENC still
		// reached AV1 through Intel QSV, which a NVIDIA-only probe would have missed
		for _, want := range []string{"nvenc", "amf", "qsv"} {
			if !strings.Contains(joined, want) {
				t.Errorf("windows must test %s", want)
			}
		}
	default:
		for _, want := range []string{"nvenc", "vaapi"} {
			if !strings.Contains(joined, want) {
				t.Errorf("linux must test %s", want)
			}
		}
	}
}

// A codec that cannot open must surface the encoder's own message, because that message is
// what distinguishes "wrong GPU generation" from "driver too old" - the two real cases seen
// on this fleet.
func TestTestEncodeReportsFailureReason(t *testing.T) {
	ff := ffmpegPath()
	if ff == "" {
		t.Skip("ffmpeg not installed on this machine")
	}
	_, err := testEncode(ff, "definitely_not_a_codec", 2, 64, 64, 10*time.Second)
	if err == nil {
		t.Fatal("a bogus codec must fail")
	}
	if strings.TrimSpace(err.Error()) == "" {
		t.Error("failure must carry a reason, not an empty string")
	}
}

// A tiny real encode must produce a positive fps; zero would silently read as "no headroom".
func TestTestEncodeSoftwareBaseline(t *testing.T) {
	ff := ffmpegPath()
	if ff == "" {
		t.Skip("ffmpeg not installed on this machine")
	}
	fps, err := testEncode(ff, "libx264", 10, 320, 240, 20*time.Second)
	if err != nil {
		t.Fatalf("software baseline must always encode: %v", err)
	}
	if fps <= 0 {
		t.Fatalf("fps must be positive, got %v", fps)
	}
}
