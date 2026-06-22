package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempGLB(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "model.glb")
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatalf("write temp glb: %v", err)
	}
	return p
}

// clearKeyEnv removes any ambient API key so key-resolution tests are deterministic.
func clearKeyEnv(t *testing.T) {
	t.Helper()
	t.Setenv(apiKeyEnv, "")
	t.Setenv(apiKeyEnvFallback, "")
	t.Setenv(apiURLEnv, "")
}

// ---------------------------------------------------------------------------
// pure helpers
// ---------------------------------------------------------------------------

func TestResolveKey(t *testing.T) {
	clearKeyEnv(t)
	if got := resolveKey("flagkey"); got != "flagkey" {
		t.Errorf("flag should win: got %q", got)
	}
	t.Setenv(apiKeyEnv, "envkey")
	if got := resolveKey(""); got != "envkey" {
		t.Errorf("GTAX_API_KEY: got %q", got)
	}
	t.Setenv(apiKeyEnv, "")
	t.Setenv(apiKeyEnvFallback, "fallbackkey")
	if got := resolveKey(""); got != "fallbackkey" {
		t.Errorf("GLB_API_KEY fallback: got %q", got)
	}
	t.Setenv(apiKeyEnvFallback, "")
	if got := resolveKey(""); got != "" {
		t.Errorf("no key anywhere: got %q", got)
	}
}

func TestResolveBase(t *testing.T) {
	clearKeyEnv(t)
	if got := resolveBase("http://flag"); got != "http://flag" {
		t.Errorf("flag should win: got %q", got)
	}
	t.Setenv(apiURLEnv, "http://env")
	if got := resolveBase(""); got != "http://env" {
		t.Errorf("GTAX_API_URL: got %q", got)
	}
	t.Setenv(apiURLEnv, "")
	if got := resolveBase(""); got != defaultAPIBase {
		t.Errorf("default: got %q, want %q", got, defaultAPIBase)
	}
}

func TestOutputNameForConvert(t *testing.T) {
	cases := []struct {
		flagOut, resultName, objectName, format, want string
	}{
		{"out.zip", "server.zip", "thing", "zip", "out.zip"}, // -o wins
		{"", "server.zip", "thing", "zip", "server.zip"},     // then X-Result-Name
		{"", "", "thing", "zip", "thing.zip"},                // then object-name + .zip
		{"", "", "", "zip", "model.zip"},                     // default stem
		{"", "", "thing", "dlc-rpf", "dlc.rpf"},              // dlc-rpf naming
	}
	for _, c := range cases {
		if got := outputNameForConvert(c.flagOut, c.resultName, c.objectName, c.format); got != c.want {
			t.Errorf("outputNameForConvert(%q,%q,%q,%q) = %q, want %q",
				c.flagOut, c.resultName, c.objectName, c.format, got, c.want)
		}
	}
}

func TestClampSize(t *testing.T) {
	if clampSize(10) != 64 {
		t.Error("below min should clamp to 64")
	}
	if clampSize(5000) != 2048 {
		t.Error("above max should clamp to 2048")
	}
	if clampSize(512) != 512 {
		t.Error("in range should pass through")
	}
}

func TestErrorFromBody(t *testing.T) {
	if got := errorFromBody([]byte(`{"error":"quota exceeded"}`)); got != "quota exceeded" {
		t.Errorf("json error: got %q", got)
	}
	if got := errorFromBody([]byte("plain text")); got != "plain text" {
		t.Errorf("plain: got %q", got)
	}
	if got := errorFromBody([]byte("   ")); got != "" {
		t.Errorf("blank: got %q", got)
	}
	long := strings.Repeat("x", 600)
	if got := errorFromBody([]byte(long)); len(got) != 503 || !strings.HasSuffix(got, "...") {
		t.Errorf("overlong should truncate to 500+ellipsis, got len %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// validation via run()
// ---------------------------------------------------------------------------

func TestRunValidation(t *testing.T) {
	clearKeyEnv(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"missing -i", []string{}, 2},
		{"non-glb extension", []string{"-i", "model.obj"}, 2},
		{"inspect and render", []string{"-i", "model.glb", "-inspect", "-render"}, 2},
		{"nonexistent file", []string{"-i", "/no/such/file.glb"}, 1},
		{"version", []string{"-version"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(c.args, &stdout, &stderr); got != c.want {
				t.Errorf("run(%v) = %d, want %d (stderr=%q)", c.args, got, c.want, stderr.String())
			}
		})
	}
}

func TestRunConvertMissingKey(t *testing.T) {
	clearKeyEnv(t)
	glb := tempGLB(t, "glTFxxxx")
	var stdout, stderr bytes.Buffer
	// No -api-key and no env key: convert must refuse with a usage error.
	if got := run([]string{"-i", glb, "-api-url", "http://unused"}, &stdout, &stderr); got != 2 {
		t.Fatalf("missing key should exit 2, got %d", got)
	}
	if !strings.Contains(stderr.String(), "requires an API key") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// endpoint round-trips against an httptest server
// ---------------------------------------------------------------------------

func TestRunConvert(t *testing.T) {
	clearKeyEnv(t)
	glb := tempGLB(t, "glTFbody!")
	out := filepath.Join(t.TempDir(), "result.zip")

	var gotAuth, gotPath string
	var gotFields map[string]string
	var gotFile []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("server parse multipart: %v", err)
		}
		gotFields = map[string]string{}
		for k := range r.MultipartForm.Value {
			gotFields[k] = r.FormValue(k)
		}
		f, _, err := r.FormFile("glb")
		if err != nil {
			t.Errorf("server FormFile glb: %v", err)
		} else {
			gotFile, _ = io.ReadAll(f)
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("X-Result-Name", "thing.zip")
		w.Header().Set("X-Total-Triangles", "1234")
		w.Header().Set("X-Total-Vertices", "567")
		w.Header().Set("X-RateLimit-Remaining", "2")
		_, _ = w.Write([]byte("PK\x03\x04zip"))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-i", glb, "-api-url", srv.URL, "-api-key", "test-key", "-o", out,
		"-format", "fivem-zip", "-object-name", "thing", "-scale", "2.5", "-include-animations",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d (stderr=%q)", code, stderr.String())
	}

	if gotPath != "/convert" {
		t.Errorf("path = %q, want /convert", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth = %q, want Bearer test-key", gotAuth)
	}
	if string(gotFile) != "glTFbody!" {
		t.Errorf("uploaded glb = %q", gotFile)
	}
	wantFields := map[string]string{
		"objectName": "thing", "format": "fivem-zip", "scale": "2.5",
		"textureMode": "ytd", "includeAnimations": "true",
	}
	for k, v := range wantFields {
		if gotFields[k] != v {
			t.Errorf("field %s = %q, want %q", k, gotFields[k], v)
		}
	}
	// Optional archetype fields must NOT be sent when unset.
	for _, k := range []string{"archetypeFlags", "archetypeLodDistance", "archetypeHdTextureDistance"} {
		if _, ok := gotFields[k]; ok {
			t.Errorf("unset field %s should be omitted, got %q", k, gotFields[k])
		}
	}

	data, err := os.ReadFile(out)
	if err != nil || !bytes.HasPrefix(data, []byte("PK")) {
		t.Errorf("output zip not written correctly: %v / %q", err, data)
	}
	if !strings.Contains(stdout.String(), "wrote "+out) {
		t.Errorf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "1234 triangles") {
		t.Errorf("stderr should report geometry, got %q", stderr.String())
	}
}

func TestRunInspectRequiresKey(t *testing.T) {
	clearKeyEnv(t)
	glb := tempGLB(t, "glTFxxxx")
	var stdout, stderr bytes.Buffer
	// Inspect is plan-gated, so the CLI must refuse before uploading when no key is present.
	if code := run([]string{"-i", glb, "-api-url", "http://unused", "-inspect"}, &stdout, &stderr); code != 2 {
		t.Fatalf("inspect without a key should exit 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "requires an API key") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunInspectWithKey(t *testing.T) {
	clearKeyEnv(t)
	glb := tempGLB(t, "glTFxxxx")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/inspect" {
			t.Errorf("path = %q, want /inspect", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mesh_count":2,"total_triangles":20000,"total_vertices":10624,"has_animations":false,"bounding_box":{"min":[0,0,0],"max":[1,2,3]}}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"-i", glb, "-api-url", srv.URL, "-inspect", "-api-key", "gtax_dglb_k"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d (stderr=%q)", code, stderr.String())
	}
	if gotAuth != "Bearer gtax_dglb_k" {
		t.Errorf("inspect should send the bearer key, got %q", gotAuth)
	}
	var resp inspectResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("stdout is not valid JSON: %v (%q)", err, stdout.String())
	}
	if resp.MeshCount != 2 || resp.TotalTriangles != 20000 {
		t.Errorf("decoded = %+v", resp)
	}
}

func TestRunRenderRequiresKey(t *testing.T) {
	clearKeyEnv(t)
	glb := tempGLB(t, "glTFxxxx")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-i", glb, "-api-url", "http://unused", "-render"}, &stdout, &stderr); code != 2 {
		t.Fatalf("render without a key should exit 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "requires an API key") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunRender(t *testing.T) {
	clearKeyEnv(t)
	glb := tempGLB(t, "glTFxxxx")
	out := filepath.Join(t.TempDir(), "preview.png")
	pngBytes := []byte("\x89PNG\r\n\x1a\n....")

	var gotSize, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/render" {
			t.Errorf("path = %q, want /render", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		_ = r.ParseMultipartForm(10 << 20)
		gotSize = r.FormValue("size")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"-i", glb, "-api-url", srv.URL, "-render", "-size", "1024", "-o", out, "-api-key", "gtax_dglb_k"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d (stderr=%q)", code, stderr.String())
	}
	if gotAuth != "Bearer gtax_dglb_k" {
		t.Errorf("render should send the bearer key, got %q", gotAuth)
	}
	if gotSize != "1024" {
		t.Errorf("size field = %q, want 1024", gotSize)
	}
	data, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(data, pngBytes) {
		t.Errorf("png not written correctly: %v", err)
	}
}

func TestRunConvertServerError(t *testing.T) {
	clearKeyEnv(t)
	glb := tempGLB(t, "glTFxxxx")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3600")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"daily glb conversion limit reached (3/day)"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"-i", glb, "-api-url", srv.URL, "-api-key", "k", "-o", filepath.Join(t.TempDir(), "x.zip")}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("server error should exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "daily glb conversion limit reached") {
		t.Errorf("stderr should carry the server message, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "retry_after_s=3600") {
		t.Errorf("stderr should surface rate-limit headers, got %q", stderr.String())
	}
}
