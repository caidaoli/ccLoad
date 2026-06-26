package app

import (
	"net/http"
	"testing"
	"testing/fstest"

	"ccLoad/internal/version"

	"github.com/gin-gonic/gin"
)

func TestStaticFileServing(t *testing.T) {
	origFS := embedFS
	origVersion := version.Version
	defer func() {
		embedFS = origFS
		version.Version = origVersion
	}()

	root := fstest.MapFS{
		"web/index.html":       &fstest.MapFile{Data: []byte("v=__VERSION__")},
		"web/app.js":           &fstest.MapFile{Data: []byte("console.log('x')")},
		"web/manifest.json":    &fstest.MapFile{Data: []byte(`{"name":"x"}`)},
		"web/dir/index.html":   &fstest.MapFile{Data: []byte("dir=__VERSION__")},
		"web/dir/asset.css":    &fstest.MapFile{Data: []byte("body{}")},
		"web/favicon.ico":      &fstest.MapFile{Data: []byte{0x00, 0x01}},
		"web/image.png":        &fstest.MapFile{Data: []byte{0x89, 0x50, 0x4e, 0x47}},
		"web/unknown.bin":      &fstest.MapFile{Data: []byte{0x01, 0x02}},
		"web/subdir/nested.js": &fstest.MapFile{Data: []byte("x")},
	}

	SetEmbedFS(root, "web")

	r := gin.New()
	setupStaticFiles(r)

	t.Run("html_replaces_version_no_cache", func(t *testing.T) {
		version.Version = "1.2.3"
		w := serveHTTP(t, r, newRequest(http.MethodGet, "/web/index.html", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
		}
		if w.Header().Get("Cache-Control") == "" {
			t.Fatal("expected Cache-Control set for html")
		}
		if w.Body.String() != "v=1.2.3" {
			t.Fatalf("body=%q, want %q", w.Body.String(), "v=1.2.3")
		}
	})

	t.Run("static_long_cache_when_not_dev", func(t *testing.T) {
		version.Version = "1.2.3"
		w := serveHTTP(t, r, newRequest(http.MethodGet, "/web/app.js", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Fatalf("Cache-Control=%q, want long cache", got)
		}
		if got := w.Header().Get("Content-Type"); got != "application/javascript; charset=utf-8" {
			t.Fatalf("Content-Type=%q, want js", got)
		}
	})

	t.Run("manifest_short_cache_when_not_dev", func(t *testing.T) {
		version.Version = "1.2.3"
		w := serveHTTP(t, r, newRequest(http.MethodGet, "/web/manifest.json", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=3600, must-revalidate" {
			t.Fatalf("Cache-Control=%q, want short cache", got)
		}
		if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Fatalf("Content-Type=%q, want json", got)
		}
	})

	t.Run("dev_version_no_cache", func(t *testing.T) {
		version.Version = "dev"
		w := serveHTTP(t, r, newRequest(http.MethodGet, "/web/app.js", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-cache, must-revalidate" {
			t.Fatalf("Cache-Control=%q, want no-cache", got)
		}
	})

	t.Run("dir_serves_index_html", func(t *testing.T) {
		version.Version = "9.9.9"
		w := serveHTTP(t, r, newRequest(http.MethodGet, "/web/dir", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
		}
		if w.Body.String() != "dir=9.9.9" {
			t.Fatalf("body=%q, want %q", w.Body.String(), "dir=9.9.9")
		}
	})

	t.Run("path_traversal_forbidden", func(t *testing.T) {
		req := newRequest(http.MethodGet, "/web/index.html", nil)
		req.URL.Path = "/web/../x"
		w := serveHTTP(t, r, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusForbidden)
		}
	})
}

func TestGetContentType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ext  string
		want string
	}{
		{".html", "text/html; charset=utf-8"},
		{".css", "text/css; charset=utf-8"},
		{".js", "application/javascript; charset=utf-8"},
		{".json", "application/json; charset=utf-8"},
		{".png", "image/png"},
		{".jpg", "image/jpeg"},
		{".jpeg", "image/jpeg"},
		{".gif", "image/gif"},
		{".svg", "image/svg+xml"},
		{".ico", "image/x-icon"},
		{".woff", "font/woff"},
		{".woff2", "font/woff2"},
		{".ttf", "font/ttf"},
		{".eot", "application/vnd.ms-fontobject"},
		{".unknown", "application/octet-stream"},
		{"", "application/octet-stream"},
	}

	for _, tc := range cases {
		t.Run(tc.ext, func(t *testing.T) {
			got := getContentType(tc.ext)
			if got != tc.want {
				t.Errorf("getContentType(%q)=%q, want %q", tc.ext, got, tc.want)
			}
		})
	}
}
