package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// miniAppModuleRef matches a quoted, same-origin Mini App asset URL that the
// browser resolves to a stable path: ES module specifiers (`from '/miniapp/...'`,
// `import '/miniapp/...'`), `<script src>` and `<link href>`. Route paths in the
// app are unprefixed (e.g. '/chats'), and API calls live under '/api', so the
// '/miniapp/...(js|mjs|css)' shape uniquely identifies cacheable asset URLs and
// nothing else — making blanket stamping safe.
var miniAppModuleRef = regexp.MustCompile(`(["'])(/miniapp/[^"']*?\.(?:m?js|css))(["'])`)

// miniAppAssetVersion is a short content fingerprint of the whole Mini App
// directory (every file's relative path, size and mtime). It changes if and only
// if a served file changes, so it is stable across server restarts that ship no
// new assets and flips precisely on deploy.
//
// Telegram Mini App WebViews cache ES module subresources by URL and do not
// re-fetch a stable URL across launches even after the source changes; a
// `no-cache` header added later cannot evict an already-cached entry. Stamping
// every asset URL with this version forces a fresh URL on every change, which
// propagates through the entire import graph because the served JS is stamped
// too (see miniAppStaticHandler).
func miniAppAssetVersion(dir string) string {
	if dir == "" {
		return ""
	}
	var entries []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return nil
		}
		entries = append(entries, rel+"|"+
			strconv.FormatInt(info.Size(), 10)+"|"+
			strconv.FormatInt(info.ModTime().UnixNano(), 10))
		return nil
	})
	if len(entries) == 0 {
		return ""
	}
	sort.Strings(entries)
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// shouldStampMiniApp reports whether a (prefix-stripped) request path serves a
// text asset whose body may contain asset URLs worth version-stamping. Only the
// HTML entrypoint and JS modules import other assets; images, fonts and CSS are
// streamed through untouched so their bytes are never buffered or rewritten.
func shouldStampMiniApp(strippedPath string) bool {
	if strippedPath == "" || strings.HasSuffix(strippedPath, "/") {
		return true // directory index → index.html
	}
	switch strings.ToLower(path.Ext(strippedPath)) {
	case ".html", ".js", ".mjs":
		return true
	default:
		return false
	}
}

// stampMiniAppModuleRefs rewrites every same-origin asset URL in body to carry
// the version query, so the whole module graph is busted in lockstep on change.
func stampMiniAppModuleRefs(body []byte, version string) []byte {
	if version == "" {
		return body
	}
	return miniAppModuleRef.ReplaceAll(body, []byte("${1}${2}?v="+version+"${3}"))
}

// captureWriter buffers a downstream handler's response so the body can be
// transformed before it is written to the real client. It implements the
// minimal http.ResponseWriter surface; http.FileServer degrades gracefully to
// plain Write when the optional Flusher/ReaderFrom interfaces are absent.
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{header: http.Header{}, status: http.StatusOK}
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(status int) { c.status = status }

func (c *captureWriter) Write(p []byte) (int, error) { return c.body.Write(p) }
