// Command gtav-glb-to-ydr converts a GLB (glTF binary) model into a ready-to-use GTA V / FiveM
// drawable package (.ydr + .ytyp + .ytd, and a .ycd when the GLB has animation) via the gtax.dev API.
//
// Every command needs an API key:
//
//	gtav-glb-to-ydr -i model.glb                       # convert -> model.zip
//	gtav-glb-to-ydr -i model.glb -format fivem-zip     # convert -> a FiveM stream resource zip
//	gtav-glb-to-ydr -i model.glb -inspect              # metadata JSON (needs an active Basic/Pro/Ultra plan)
//	gtav-glb-to-ydr -i model.glb -render -o p.png      # isometric PNG preview (needs an active plan)
//
// Authentication uses your global gtax.dev API key (gtax_dglb_...), the same key as the other gtax
// services. Set it with -api-key or the GTAX_API_KEY environment variable. Convert needs a valid key;
// inspect and render additionally require an active Basic, Pro, or Ultra subscription.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAPIBase    = "https://public-glb-to-ydr.gtax.dev"
	apiKeyEnv         = "GTAX_API_KEY"
	apiKeyEnvFallback = "GLB_API_KEY"
	apiURLEnv         = "GTAX_API_URL"
	// maxUploadBytes is the total request-body cap. The hosted API sits behind Cloudflare's free plan,
	// which rejects uploads larger than 100 MB, so reject early with a clear message.
	maxUploadBytes = 100 * 1024 * 1024
	toolName       = "gtav-glb-to-ydr"
)

// version is overridden at release time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses flags and dispatches to convert / inspect / render. It returns a process exit code (0 ok,
// 1 runtime error, 2 usage error) instead of calling os.Exit, so tests can drive it directly.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(toolName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		input       = fs.String("i", "", "input `.glb` file (required)")
		output      = fs.String("o", "", "output path (default: derived from the result name or input)")
		inspect     = fs.Bool("inspect", false, "print the GLB's metadata as JSON, then exit (requires an active Basic/Pro/Ultra plan)")
		render      = fs.Bool("render", false, "render an isometric PNG preview instead of converting (requires an active Basic/Pro/Ultra plan)")
		objectName  = fs.String("object-name", "model", "output name stem (e.g. model.ydr)")
		format      = fs.String("format", "zip", "output package: zip, dlc-rpf, or fivem-zip")
		scale       = fs.Float64("scale", 1.0, "uniform model scale")
		lodMed      = fs.Float64("lod-med", 0, "MED LOD distance (0 = no MED LOD)")
		lodLow      = fs.Float64("lod-low", 0, "LOW LOD distance")
		lodVlow     = fs.Float64("lod-vlow", 0, "VLOW LOD distance")
		archLod     = fs.String("archetype-lod-distance", "", "override the archetype LOD distance (float)")
		archHdTex   = fs.String("archetype-hd-texture-distance", "", "override the archetype HD texture distance (float)")
		archFlags   = fs.String("archetype-flags", "", "override the archetype flags (uint32)")
		textureMode = fs.String("texture-mode", "ytd", "texture mode: ytd (separate .ytd) or embed")
		includeAnim = fs.Bool("include-animations", false, "extract the GLB's animations into a .ycd")
		size        = fs.Int("size", 768, "PNG size in pixels for -render (64..2048)")
		apiKey      = fs.String("api-key", "", "API key (or env "+apiKeyEnv+")")
		apiURL      = fs.String("api-url", "", "API base URL (or env "+apiURLEnv+", default "+defaultAPIBase+")")
		timeout     = fs.Duration("timeout", 5*time.Minute, "HTTP client timeout")
		quiet       = fs.Bool("quiet", false, "suppress progress output on stderr")
		showVersion = fs.Bool("version", false, "print the version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s -i <model.glb> [options]\n\n", toolName)
		fmt.Fprintln(stderr, "Convert a GLB model to a GTA V / FiveM drawable package, or inspect / preview it.")
		fmt.Fprintln(stderr, "\nExamples:")
		fmt.Fprintf(stderr, "  %s -i model.glb                       # convert -> model.zip\n", toolName)
		fmt.Fprintf(stderr, "  %s -i model.glb -format fivem-zip     # FiveM stream resource\n", toolName)
		fmt.Fprintf(stderr, "  %s -i model.glb -include-animations   # bake animations into a .ycd\n", toolName)
		fmt.Fprintf(stderr, "  %s -i model.glb -inspect              # metadata JSON (needs an active plan)\n", toolName)
		fmt.Fprintf(stderr, "  %s -i model.glb -render -o p.png      # PNG preview (needs an active plan)\n", toolName)
		fmt.Fprintln(stderr, "\nOptions:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) { // -h / -help: usage already printed
			return 0
		}
		return 2
	}

	if *showVersion {
		fmt.Fprintf(stdout, "%s %s\n", toolName, version)
		return 0
	}

	if strings.TrimSpace(*input) == "" {
		fmt.Fprintln(stderr, "error: -i is required")
		fs.Usage()
		return 2
	}
	if *inspect && *render {
		fmt.Fprintln(stderr, "error: -inspect and -render are mutually exclusive")
		return 2
	}
	if ext := strings.ToLower(filepath.Ext(*input)); ext != ".glb" {
		fmt.Fprintf(stderr, "error: input must be a .glb file, got %q\n", ext)
		return 2
	}

	info, err := os.Stat(*input)
	if err != nil {
		fmt.Fprintf(stderr, "error: cannot read input: %v\n", err)
		return 1
	}
	if info.IsDir() {
		fmt.Fprintf(stderr, "error: input %q is a directory\n", *input)
		return 2
	}
	if info.Size() > maxUploadBytes {
		fmt.Fprintf(stderr, "error: input is %d bytes, over the %d MB upload limit\n", info.Size(), maxUploadBytes/(1024*1024))
		return 2
	}

	base := resolveBase(*apiURL)
	key := resolveKey(*apiKey)
	client := newClient(*timeout)

	switch {
	case *inspect:
		if key == "" {
			fmt.Fprintf(stderr, "error: -inspect requires an API key on an active Basic/Pro/Ultra plan. Set -api-key or %s (your gtax_dglb_ key from gtax.dev/api-keys).\n", apiKeyEnv)
			return 2
		}
		return runInspect(client, base, *input, key, *quiet, stdout, stderr)
	case *render:
		if key == "" {
			fmt.Fprintf(stderr, "error: -render requires an API key on an active Basic/Pro/Ultra plan. Set -api-key or %s (your gtax_dglb_ key from gtax.dev/api-keys).\n", apiKeyEnv)
			return 2
		}
		return runRender(client, base, *input, *output, clampSize(*size), key, *quiet, stdout, stderr)
	default:
		if key == "" {
			fmt.Fprintf(stderr, "error: convert requires an API key. Set -api-key or %s (your gtax_dglb_ key from gtax.dev/api-keys).\n", apiKeyEnv)
			return 2
		}
		formatVal := strings.ToLower(strings.TrimSpace(*format))
		if !validConvertFormat(formatVal) {
			fmt.Fprintf(stderr, "error: -format must be zip, dlc-rpf, or fivem-zip, got %q\n", *format)
			return 2
		}
		texMode := strings.ToLower(strings.TrimSpace(*textureMode))
		if texMode != "ytd" && texMode != "embed" {
			fmt.Fprintf(stderr, "error: -texture-mode must be ytd or embed, got %q\n", *textureMode)
			return 2
		}
		if code := validateArchetypeFlags(*archLod, *archHdTex, *archFlags, stderr); code != 0 {
			return code
		}
		req := convertRequest{
			input:             *input,
			output:            *output,
			objectName:        strings.TrimSpace(*objectName),
			format:            formatVal,
			scale:             *scale,
			lodMed:            *lodMed,
			lodLow:            *lodLow,
			lodVlow:           *lodVlow,
			archLodDistance:   strings.TrimSpace(*archLod),
			archHdTexDistance: strings.TrimSpace(*archHdTex),
			archFlags:         strings.TrimSpace(*archFlags),
			textureMode:       texMode,
			includeAnims:      *includeAnim,
		}
		return runConvert(client, base, req, key, *quiet, stdout, stderr)
	}
}

func clampSize(n int) int {
	if n < 64 {
		return 64
	}
	if n > 2048 {
		return 2048
	}
	return n
}

func validConvertFormat(f string) bool {
	switch f {
	case "zip", "dlc-rpf", "fivem-zip":
		return true
	default:
		return false
	}
}

// validateArchetypeFlags rejects malformed numeric overrides up front, since the server silently
// ignores unparseable archetype values. Returns 0 when everything is valid (or unset).
func validateArchetypeFlags(lod, hdTex, flags string, stderr io.Writer) int {
	if v := strings.TrimSpace(lod); v != "" {
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			fmt.Fprintf(stderr, "error: -archetype-lod-distance must be a number, got %q\n", v)
			return 2
		}
	}
	if v := strings.TrimSpace(hdTex); v != "" {
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			fmt.Fprintf(stderr, "error: -archetype-hd-texture-distance must be a number, got %q\n", v)
			return 2
		}
	}
	if v := strings.TrimSpace(flags); v != "" {
		if _, err := strconv.ParseUint(v, 10, 32); err != nil {
			fmt.Fprintf(stderr, "error: -archetype-flags must be a uint32, got %q\n", v)
			return 2
		}
	}
	return 0
}
