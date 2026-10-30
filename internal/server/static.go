package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

type staticFile struct {
	contentType string
	etag        string
	cache       string
	plain       []byte
	gzipped     []byte
}

// staticHandler serves the embedded UI from memory. Everything is compressed once at
// startup, and browsers revalidate with an etag, so a page load after the first one only
// costs a few empty responses. Fonts never change and can be cached for good.
type staticHandler struct {
	files map[string]*staticFile
}

func newStaticHandler(fsys fs.FS) *staticHandler {
	h := &staticHandler{files: map[string]*staticFile{}}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		f := &staticFile{
			contentType: mime.TypeByExtension(path.Ext(name)),
			etag:        `"` + hex.EncodeToString(sum[:8]) + `"`,
			cache:       "no-cache",
			plain:       data,
		}
		switch path.Ext(name) {
		case ".js":
			f.contentType = "text/javascript; charset=utf-8" // some systems map it to something else
		case ".woff2":
			f.contentType = "font/woff2"
			f.cache = "public, max-age=31536000, immutable"
		}
		if f.contentType == "" {
			f.contentType = "application/octet-stream"
		}
		if path.Ext(name) != ".woff2" {
			var buf bytes.Buffer
			gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			gz.Write(data)
			gz.Close()
			if buf.Len() < len(data) {
				f.gzipped = buf.Bytes()
			}
		}
		h.files[name] = f
		return nil
	})
	if err != nil {
		panic(err) // the files are compiled in, this cannot fail on a build that passed its tests
	}
	return h
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	f := h.files[name]
	if f == nil {
		http.NotFound(w, r)
		return
	}
	header := w.Header()
	header.Set("Content-Type", f.contentType)
	header.Set("ETag", f.etag)
	header.Set("Cache-Control", f.cache)
	header.Set("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := f.plain
	if f.gzipped != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		header.Set("Content-Encoding", "gzip")
		body = f.gzipped
	}
	if r.Method == http.MethodGet {
		w.Write(body)
	}
}
