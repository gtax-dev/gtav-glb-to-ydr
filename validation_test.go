package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// pure validators
// ---------------------------------------------------------------------------

func TestValidConvertFormat(t *testing.T) {
	for _, f := range []string{"zip", "dlc-rpf", "fivem-zip"} {
		if !validConvertFormat(f) {
			t.Errorf("%q should be valid", f)
		}
	}
	for _, f := range []string{"", "dlc", "rpf", "ZIP", "fivem", "zip "} {
		if validConvertFormat(f) {
			t.Errorf("%q should be invalid", f)
		}
	}
}

func TestValidateArchetypeFlags(t *testing.T) {
	var buf bytes.Buffer
	if validateArchetypeFlags("", "", "", &buf) != 0 {
		t.Error("all empty should pass")
	}
	if validateArchetypeFlags("100.5", "40", "512", &buf) != 0 {
		t.Error("valid numbers should pass")
	}
	cases := [][3]string{
		{"abc", "", ""}, // bad lod distance
		{"", "xx", ""},  // bad hd texture distance
		{"", "", "-1"},  // flags must be a uint32
		{"", "", "1.5"}, // flags must be an integer
		{"", "", "abc"}, // flags not a number
	}
	for _, c := range cases {
		if validateArchetypeFlags(c[0], c[1], c[2], &buf) != 2 {
			t.Errorf("validateArchetypeFlags(%q,%q,%q) should fail", c[0], c[1], c[2])
		}
	}
}

// ---------------------------------------------------------------------------
// flag validation through run()
// ---------------------------------------------------------------------------

func TestRunHelpExitsZero(t *testing.T) {
	clearKeyEnv(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Errorf("-h should exit 0, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("-h should print usage, stderr=%q", stderr.String())
	}
}

func TestRunConvertRejectsBadOptions(t *testing.T) {
	clearKeyEnv(t)
	glb := tempGLB(t, "glTFxxxx")
	cases := []struct {
		name string
		args []string
		msg  string
	}{
		{"bad format", []string{"-format", "dlc"}, "-format must be"},
		{"bad texture-mode", []string{"-texture-mode", "bogus"}, "-texture-mode must be"},
		{"bad archetype-flags", []string{"-archetype-flags", "abc"}, "-archetype-flags must be"},
		{"bad archetype-lod", []string{"-archetype-lod-distance", "xx"}, "-archetype-lod-distance must be"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"-i", glb, "-api-key", "k"}, c.args...)
			if code := run(args, &stdout, &stderr); code != 2 {
				t.Fatalf("expected exit 2, got %d (stderr=%q)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), c.msg) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), c.msg)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// -quiet behaviour
// ---------------------------------------------------------------------------

func TestRunQuietSuppressesRateLimitOnSuccess(t *testing.T) {
	clearKeyEnv(t)
	glb := tempGLB(t, "glTFxxxx")
	out := filepath.Join(t.TempDir(), "o.zip")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("X-RateLimit-Remaining", "2")
		w.Header().Set("X-Total-Triangles", "10")
		_, _ = w.Write([]byte("PKzip"))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"-i", glb, "-api-url", srv.URL, "-api-key", "k", "-o", out, "-quiet"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d (stderr=%q)", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "rate_remaining") {
		t.Errorf("-quiet should suppress the success rate-limit footer, stderr=%q", stderr.String())
	}
	if strings.Contains(stderr.String(), "Converting") || strings.Contains(stderr.String(), "triangles") {
		t.Errorf("-quiet should suppress progress, stderr=%q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "wrote "+out) {
		t.Errorf("the output path should still be printed, stdout=%q", stdout.String())
	}
}

func TestRunQuietStillShowsErrorDetail(t *testing.T) {
	clearKeyEnv(t)
	glb := tempGLB(t, "glTFxxxx")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1800")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"quota exceeded"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"-i", glb, "-api-url", srv.URL, "-api-key", "k", "-o", filepath.Join(t.TempDir(), "x.zip"), "-quiet"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	// Even under -quiet, an error and its rate-limit context must surface.
	if !strings.Contains(stderr.String(), "quota exceeded") {
		t.Errorf("error message missing under -quiet, stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "retry_after_s=1800") {
		t.Errorf("error rate-limit missing under -quiet, stderr=%q", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// optional convert fields are forwarded when valid
// ---------------------------------------------------------------------------

func TestRunConvertSendsOptionalFields(t *testing.T) {
	clearKeyEnv(t)
	glb := tempGLB(t, "glTFxxxx")
	var fields map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(10 << 20)
		fields = map[string]string{}
		for k := range r.MultipartForm.Value {
			fields[k] = r.FormValue(k)
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write([]byte("PK"))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-i", glb, "-api-url", srv.URL, "-api-key", "k", "-o", filepath.Join(t.TempDir(), "o.zip"),
		"-archetype-flags", "512", "-archetype-lod-distance", "150.5", "-archetype-hd-texture-distance", "40",
		"-texture-mode", "embed", "-lod-med", "60",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d (stderr=%q)", code, stderr.String())
	}
	want := map[string]string{
		"archetypeFlags": "512", "archetypeLodDistance": "150.5", "archetypeHdTextureDistance": "40",
		"textureMode": "embed", "lodMed": "60",
	}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("field %s = %q, want %q", k, fields[k], v)
		}
	}
}

func TestRunRenderClampsSize(t *testing.T) {
	clearKeyEnv(t)
	glb := tempGLB(t, "glTFxxxx")
	var gotSize string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(10 << 20)
		gotSize = r.FormValue("size")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG"))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	run([]string{"-i", glb, "-api-url", srv.URL, "-render", "-size", "99999", "-o", filepath.Join(t.TempDir(), "p.png"), "-api-key", "k"}, &stdout, &stderr)
	if gotSize != "2048" {
		t.Errorf("-size should clamp to 2048, server saw %q", gotSize)
	}
}

// ---------------------------------------------------------------------------
// streamToFile cleanup
// ---------------------------------------------------------------------------

type errReader struct {
	data []byte
	pos  int
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.pos < len(e.data) {
		n := copy(p, e.data[e.pos:])
		e.pos += n
		return n, nil
	}
	return 0, errors.New("read boom")
}

func TestStreamToFileRemovesPartialOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.bin")
	if _, err := streamToFile(path, &errReader{data: []byte("somebytes")}); err == nil {
		t.Fatal("expected a write error")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("a partial file should be removed on error, stat err = %v", statErr)
	}
}

func TestStreamToFileWritesAndReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.bin")
	n, err := streamToFile(path, strings.NewReader("hello world"))
	if err != nil {
		t.Fatalf("streamToFile: %v", err)
	}
	if n != 11 {
		t.Errorf("byte count = %d, want 11", n)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "hello world" {
		t.Errorf("file content = %q", data)
	}
}
