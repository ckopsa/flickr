package pipeline

// Hardware-acceleration detection. Lives in the pipeline package because
// which encoder to use is a pipeline concern — the decision engine says
// "re-encode to h264"; this module figures out h264_qsv vs h264_vaapi vs
// libx264 on this particular machine.
//
// An encoder being compiled into ffmpeg does NOT mean it works (nvenc is
// often present without an NVIDIA driver), so every candidate is verified
// with a short test encode. The full detection trace is kept and exposed
// over the API: "why is it (not) using my GPU?" should be answerable
// without reading server logs.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type HWAccel struct {
	Kind     string            `json:"kind"`             // "nvenc", "qsv", "vaapi", "v4l2m2m"
	Device   string            `json:"device,omitempty"` // e.g. /dev/dri/renderD128
	Encoders map[string]string `json:"encoders"`         // target codec -> ffmpeg encoder
}

type DetectStep struct {
	Candidate string `json:"candidate"`
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
}

type HWReport struct {
	Chosen *HWAccel     `json:"chosen"` // nil = software encoding
	Trace  []DetectStep `json:"trace"`
}

// DetectHW probes hardware encoders in preference order and returns the
// first that survives a verification encode, plus the full trace.
func DetectHW(ctx context.Context) *HWReport {
	report := &HWReport{}
	compiled := compiledEncoders(ctx)
	renderNodes, _ := filepath.Glob("/dev/dri/renderD*")

	type candidate struct {
		kind      string
		device    string
		testArgs  func(enc, device string) []string
		available bool
		reason    string
	}
	candidates := []candidate{
		{
			kind:      "nvenc",
			available: compiled["h264_nvenc"],
			reason:    "h264_nvenc not compiled into ffmpeg",
			testArgs: func(enc, _ string) []string {
				return []string{"-c:v", enc}
			},
		},
		{
			kind:      "qsv",
			available: compiled["h264_qsv"] && len(renderNodes) > 0,
			reason:    "needs h264_qsv encoder and a /dev/dri render node",
			testArgs: func(enc, _ string) []string {
				return []string{"-vf", "format=nv12", "-c:v", enc}
			},
		},
		{
			kind:      "vaapi",
			available: compiled["h264_vaapi"] && len(renderNodes) > 0,
			reason:    "needs h264_vaapi encoder and a /dev/dri render node",
			testArgs: func(enc, device string) []string {
				return []string{"-vf", "format=nv12,hwupload", "-c:v", enc}
			},
		},
		{
			kind:      "rkmpp",
			available: compiled["h264_rkmpp"] && pathExists("/dev/mpp_service"),
			reason:    "needs h264_rkmpp encoder and /dev/mpp_service (Rockchip VPU)",
			testArgs: func(enc, _ string) []string {
				return []string{"-c:v", enc}
			},
		},
		{
			kind:      "v4l2m2m",
			available: compiled["h264_v4l2m2m"],
			reason:    "h264_v4l2m2m not compiled into ffmpeg",
			testArgs: func(enc, _ string) []string {
				return []string{"-c:v", enc}
			},
		},
	}

	device := ""
	if len(renderNodes) > 0 {
		device = renderNodes[0]
	}
	for _, c := range candidates {
		if !c.available {
			report.Trace = append(report.Trace, DetectStep{c.kind, false, c.reason})
			continue
		}
		enc := "h264_" + c.kind
		if err := testEncode(ctx, c.kind, enc, device, c.testArgs(enc, device)); err != nil {
			report.Trace = append(report.Trace, DetectStep{c.kind, false,
				fmt.Sprintf("test encode with %s failed: %s", enc, err)})
			continue
		}
		accel := &HWAccel{Kind: c.kind, Device: device, Encoders: map[string]string{"h264": enc}}
		if compiled["hevc_"+c.kind] {
			accel.Encoders["hevc"] = "hevc_" + c.kind
		}
		report.Trace = append(report.Trace, DetectStep{c.kind, true,
			fmt.Sprintf("verified with test encode (%s)", strings.Join(mapValues(accel.Encoders), ", "))})
		if report.Chosen == nil {
			report.Chosen = accel
		}
	}
	if report.Chosen == nil {
		report.Trace = append(report.Trace, DetectStep{"software", true,
			"no hardware encoder verified; using libx264/libx265"})
	}
	return report
}

// testEncode runs a ~0.3s synthetic encode to prove the encoder actually
// initializes on this machine (drivers, devices, permissions and all).
func testEncode(ctx context.Context, kind, enc, device string, tailArgs []string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-v", "error"}
	args = append(args, hwInitArgs(kind, device)...)
	args = append(args, "-f", "lavfi", "-i", "testsrc2=duration=0.3:size=320x240:rate=30")
	args = append(args, tailArgs...)
	args = append(args, "-f", "null", "-")
	out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// hwInitArgs returns the global ffmpeg args a hardware kind needs before -i.
func hwInitArgs(kind, device string) []string {
	switch kind {
	case "qsv":
		return []string{"-init_hw_device", "qsv=hw"}
	case "vaapi":
		return []string{"-init_hw_device", "vaapi=va:" + device, "-filter_hw_device", "va"}
	default:
		return nil
	}
}

// presetArgs returns the encoder-appropriate speed preset — preset
// namespaces differ per encoder family (libx264 "veryfast" vs nvenc "p5").
func presetArgs(encoder string) []string {
	switch {
	case strings.HasSuffix(encoder, "_nvenc"):
		return []string{"-preset", "p5"}
	case strings.HasSuffix(encoder, "_vaapi"), strings.HasSuffix(encoder, "_v4l2m2m"),
		strings.HasSuffix(encoder, "_rkmpp"):
		return nil // no preset concept
	default: // libx264, libx265, qsv all accept the x264-style names
		return []string{"-preset", "veryfast"}
	}
}

// ResolveEncoder maps a target codec to the encoder BuildArgs will pick,
// so callers can report it (observability) without duplicating the logic.
func ResolveEncoder(hw *HWAccel, codec string) string {
	if hw != nil {
		if enc, ok := hw.Encoders[codec]; ok {
			return enc
		}
	}
	return encoderFor(codec)
}

func compiledEncoders(ctx context.Context) map[string]bool {
	out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "V") {
			set[fields[1]] = true
		}
	}
	return set
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
