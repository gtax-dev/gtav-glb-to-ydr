package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// api.go handles all communication with the gtax.dev GLB to YDR API: auth/URL resolution, the streaming
// multipart upload, the three endpoint flows (convert / inspect / render), and response handling.

// convertRequest carries the resolved /convert options from the flag layer.
type convertRequest struct {
	input             string
	output            string
	objectName        string
	format            string
	scale             float64
	lodMed            float64
	lodLow            float64
	lodVlow           float64
	archLodDistance   string // empty = not sent (server picks a default)
	archHdTexDistance string
	archFlags         string
	textureMode       string
	includeAnims      bool
}

// inspectResponse mirrors the JSON returned by POST /inspect. The CLI pretty-prints the raw bytes; this
// type documents the shape and is decoded in tests.
type inspectResponse struct {
	MeshCount      int               `json:"mesh_count"`
	PrimitiveCount int               `json:"primitive_count"`
	TotalVertices  int               `json:"total_vertices"`
	TotalTriangles int               `json:"total_triangles"`
	Materials      []inspectMaterial `json:"materials"`
	Textures       []string          `json:"textures"`
	HasSkins       bool              `json:"has_skins"`
	SkinCount      int               `json:"skin_count"`
	HasAnimations  bool              `json:"has_animations"`
	Animations     []inspectAnim     `json:"animations"`
	BoundingBox    struct {
		Min [3]float64 `json:"min"`
		Max [3]float64 `json:"max"`
	} `json:"bounding_box"`
}

type inspectMaterial struct {
	Index       int    `json:"index"`
	Name        string `json:"name,omitempty"`
	TextureName string `json:"texture_name,omitempty"`
}

type inspectAnim struct {
	Index    int     `json:"index"`
	Name     string  `json:"name,omitempty"`
	Channels int     `json:"channels"`
	Duration float64 `json:"duration"`
}

type apiError struct {
	Error string `json:"error"`
}

func resolveKey(flagVal string) string {
	if k := strings.TrimSpace(flagVal); k != "" {
		return k
	}
	if k := strings.TrimSpace(os.Getenv(apiKeyEnv)); k != "" {
		return k
	}
	return strings.TrimSpace(os.Getenv(apiKeyEnvFallback))
}

func resolveBase(flagVal string) string {
	if b := strings.TrimSpace(flagVal); b != "" {
		return b
	}
	if b := strings.TrimSpace(os.Getenv(apiURLEnv)); b != "" {
		return b
	}
	return defaultAPIBase
}

func newClient(timeout time.Duration) *http.Client { return &http.Client{Timeout: timeout} }

func apiURL(base, path string) string { return strings.TrimRight(base, "/") + path }

// uploadGLB streams a multipart POST: the `glb` file part (piped straight from disk, never fully
// buffered) followed by the string fields. The Authorization header is set only when apiKey is present.
func uploadGLB(client *http.Client, url, glbPath string, fields map[string]string, apiKey string) (*http.Response, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		err := func() error {
			f, err := os.Open(glbPath)
			if err != nil {
				return err
			}
			defer f.Close()
			part, err := mw.CreateFormFile("glb", filepath.Base(glbPath))
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			for k, v := range fields {
				if err := mw.WriteField(k, v); err != nil {
					return err
				}
			}
			return nil
		}()
		if cerr := mw.Close(); err == nil {
			err = cerr
		}
		_ = pw.CloseWithError(err)
	}()

	req, err := http.NewRequest(http.MethodPost, url, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return client.Do(req)
}

// runConvert POSTs /convert and writes the returned archive to disk.
func runConvert(client *http.Client, base string, req convertRequest, key string, quiet bool, stdout, stderr io.Writer) int {
	logf := newLogf(quiet, stderr)
	format := defaultStr(req.format, "zip")
	fields := map[string]string{
		"objectName":  defaultStr(req.objectName, "model"),
		"format":      format,
		"scale":       formatFloat(req.scale),
		"lodMed":      formatFloat(req.lodMed),
		"lodLow":      formatFloat(req.lodLow),
		"lodVlow":     formatFloat(req.lodVlow),
		"textureMode": defaultStr(req.textureMode, "ytd"),
	}
	if req.includeAnims {
		fields["includeAnimations"] = "true"
	}
	if req.archLodDistance != "" {
		fields["archetypeLodDistance"] = req.archLodDistance
	}
	if req.archHdTexDistance != "" {
		fields["archetypeHdTextureDistance"] = req.archHdTexDistance
	}
	if req.archFlags != "" {
		fields["archetypeFlags"] = req.archFlags
	}

	logf("Converting %s (%s) via %s ...\n", filepath.Base(req.input), format, base)
	resp, err := uploadGLB(client, apiURL(base, "/convert"), req.input, fields, key)
	if err != nil {
		fmt.Fprintf(stderr, "error: request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "error: %s: %s\n", resp.Status, readErrorBody(resp))
		printRateLimit(stderr, resp.Header)
		return 1
	}

	out := outputNameForConvert(req.output, resp.Header.Get("X-Result-Name"), req.objectName, format)
	n, err := streamToFile(out, resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if tri := resp.Header.Get("X-Total-Triangles"); tri != "" {
		logf("geometry: %s triangles, %s vertices\n", tri, resp.Header.Get("X-Total-Vertices"))
	}
	if !quiet {
		printRateLimit(stderr, resp.Header)
	}
	fmt.Fprintf(stdout, "wrote %s (%d bytes)\n", out, n)
	return 0
}

// runInspect POSTs /inspect and pretty-prints the metadata JSON to stdout. No API key is required.
func runInspect(client *http.Client, base, input, key string, quiet bool, stdout, stderr io.Writer) int {
	resp, err := uploadGLB(client, apiURL(base, "/inspect"), input, nil, key)
	if err != nil {
		fmt.Fprintf(stderr, "error: request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "error: read response: %v\n", err)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "error: %s: %s\n", resp.Status, errorFromBody(body))
		printRateLimit(stderr, resp.Header)
		return 1
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") == nil {
		fmt.Fprintln(stdout, pretty.String())
	} else {
		fmt.Fprintln(stdout, strings.TrimSpace(string(body)))
	}
	if !quiet {
		printRateLimit(stderr, resp.Header)
	}
	return 0
}

// runRender POSTs /render and writes the PNG preview to disk. No API key is required.
func runRender(client *http.Client, base, input, output string, size int, key string, quiet bool, stdout, stderr io.Writer) int {
	logf := newLogf(quiet, stderr)
	logf("Rendering %s at %dpx via %s ...\n", filepath.Base(input), size, base)
	resp, err := uploadGLB(client, apiURL(base, "/render"), input, map[string]string{"size": strconv.Itoa(size)}, key)
	if err != nil {
		fmt.Fprintf(stderr, "error: request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "error: %s: %s\n", resp.Status, readErrorBody(resp))
		printRateLimit(stderr, resp.Header)
		return 1
	}
	out := strings.TrimSpace(output)
	if out == "" {
		out = strings.TrimSuffix(filepath.Base(input), filepath.Ext(input)) + ".png"
	}
	n, err := streamToFile(out, resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if !quiet {
		printRateLimit(stderr, resp.Header)
	}
	fmt.Fprintf(stdout, "wrote %s (%d bytes)\n", out, n)
	return 0
}

// newLogf returns a printf-style logger that writes to w unless quiet is set.
func newLogf(quiet bool, w io.Writer) func(string, ...any) {
	return func(f string, a ...any) {
		if !quiet {
			fmt.Fprintf(w, f, a...)
		}
	}
}

// outputNameForConvert resolves the convert output path: the -o flag wins, then the server's
// X-Result-Name, then a name derived from the object name + format.
func outputNameForConvert(flagOut, resultName, objectName, format string) string {
	if v := strings.TrimSpace(flagOut); v != "" {
		return v
	}
	if v := strings.TrimSpace(resultName); v != "" {
		return v
	}
	name := strings.TrimSpace(objectName)
	if name == "" {
		name = "model"
	}
	if format == "dlc-rpf" {
		return "dlc.rpf"
	}
	return name + ".zip"
}

func streamToFile(path string, r io.Reader) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("create output %s: %w", path, err)
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(path) // don't leave a truncated archive behind
		return n, fmt.Errorf("write output %s: %w", path, copyErr)
	}
	if closeErr != nil {
		return n, fmt.Errorf("close output %s: %w", path, closeErr)
	}
	return n, nil
}

func readErrorBody(resp *http.Response) string {
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("(read body: %v)", err)
	}
	return errorFromBody(b)
}

// errorFromBody extracts a human-readable message, preferring the JSON {"error":"..."} shape and
// otherwise truncating the raw body.
func errorFromBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	var e apiError
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}

func printRateLimit(w io.Writer, h http.Header) {
	if h == nil {
		return
	}
	var parts []string
	add := func(label, key string) {
		if v := h.Get(key); v != "" {
			parts = append(parts, label+"="+v)
		}
	}
	add("rate_remaining", "X-RateLimit-Remaining")
	add("rate_limit", "X-RateLimit-Limit")
	add("retry_after_s", "Retry-After")
	add("queue_wait_ms", "X-Queue-Wait-Ms")
	if len(parts) > 0 {
		fmt.Fprintln(w, strings.Join(parts, " "))
	}
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
