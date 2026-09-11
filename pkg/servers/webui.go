package servers

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/webui"
)

// The interface is served from "/", which the mux treats as the pattern that
// matches everything no other pattern claims. An unmatched API path therefore
// arrives here too, and answering it with HTML would turn a typo in a route
// into a 200 that no client can parse. apiNamespaces is what keeps that from
// happening.
var apiNamespaces = []string{"/v1/", "/mcp", "/health"}

// browserRoutes are the paths the interface routes itself, so a request under
// one of them is always the document.
//
// The extension rule in lookupAsset cannot decide these. A conversation's path
// carries a session id, the id comes from a caller, and one that contains a dot
// would look like a file name and be reported as a missing file.
var browserRoutes = []string{"/c/"}

// webAsset is one embedded file with the validators computed once.
type webAsset struct {
	content     []byte
	etag        string
	contentType string
}

// assetCache holds the embedded files keyed by their request path. The set is
// fixed at compile time, so it is read once and never invalidated.
var (
	assetOnce  sync.Once
	assetsByID map[string]webAsset
)

// loadAssets reads every embedded file and computes its validator.
func loadAssets() {
	assetsByID = make(map[string]webAsset)
	files := webui.Files()
	err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		contentType := mime.TypeByExtension(path.Ext(name))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		assetsByID["/"+name] = webAsset{
			content:     content,
			etag:        `"` + hex.EncodeToString(sum[:]) + `"`,
			contentType: contentType,
		}
		return nil
	})
	if err != nil {
		logging.Errorf("webui: cannot read the embedded interface: %v", err)
	}
}

// lookupAsset resolves a request path to an embedded file.
//
// A path that names no file falls back to the document, because the interface
// routes in the browser and a reload on one of its own routes must not 404. A
// path that carries an extension does not fall back, so a missing script is
// reported as missing instead of being answered with HTML.
func lookupAsset(requestPath string) (webAsset, bool) {
	assetOnce.Do(loadAssets)

	clean := path.Clean("/" + strings.TrimPrefix(requestPath, "/"))
	if clean == "/" {
		clean = "/index.html"
	}
	if asset, ok := assetsByID[clean]; ok {
		return asset, true
	}
	if !isBrowserRoute(clean) && path.Ext(clean) != "" {
		return webAsset{}, false
	}
	asset, ok := assetsByID["/index.html"]
	return asset, ok
}

// isBrowserRoute reports whether a cleaned path belongs to the interface's own
// routing rather than to a file.
func isBrowserRoute(clean string) bool {
	for _, route := range browserRoutes {
		if clean == strings.TrimSuffix(route, "/") || strings.HasPrefix(clean, route) {
			return true
		}
	}
	return false
}

// cacheControlFor returns the caching rule for one path.
//
// Vite writes a content hash into every asset file name, so those may be held
// indefinitely; the document names them and must be revalidated every time or
// a deploy would keep serving the previous build.
func cacheControlFor(requestPath string) string {
	if immutableAsset(requestPath) {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

// immutableAsset reports whether a path names a build output that may be held
// indefinitely. Vite writes a content hash into each of these file names, so a
// build that changes the bytes changes the name with them.
func immutableAsset(requestPath string) bool {
	return strings.HasPrefix(requestPath, "/assets/")
}

// servesWebUI reports whether this request belongs to the interface.
//
// "/" is the mux fallback pattern, so an unmatched API path arrives at the
// interface handler too. It has to stay a 404 rather than be answered with
// HTML no API client can parse.
func (api *APIServer) servesWebUI(r *http.Request) bool {
	if !api.config.EnableWebUI {
		return false
	}
	for _, namespace := range apiNamespaces {
		if strings.HasPrefix(r.URL.Path, namespace) || r.URL.Path == strings.TrimSuffix(namespace, "/") {
			return false
		}
	}
	return true
}

// handleWebUI serves the browser interface.
//
// The document is served without an API key, because the screen that asks for
// the key cannot itself require one. Every data call the interface makes stays
// behind withAuth.
func (api *APIServer) handleWebUI(w http.ResponseWriter, r *http.Request) {
	if !api.servesWebUI(r) {
		api.sendError(w, http.StatusNotFound, "Not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	asset, ok := lookupAsset(r.URL.Path)
	if !ok {
		api.sendError(w, http.StatusNotFound, "Not found")
		return
	}

	w.Header().Set("Cache-Control", cacheControlFor(r.URL.Path))
	// An immutable asset needs no validator. Its name carries the hash of its
	// bytes, so a conditional request on it can only ever confirm the file the
	// client already holds, and a tag invites that round-trip for nothing.
	if !immutableAsset(r.URL.Path) {
		w.Header().Set("ETag", asset.etag)
		if matchesETag(r.Header.Get("If-None-Match"), asset.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	w.Header().Set("Content-Type", asset.contentType)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(asset.content); err != nil {
		logging.Debugf("webui: write failed for %s: %v", r.URL.Path, err)
	}
}

// matchesETag reports whether an If-None-Match header covers the given tag.
// The header may list several tags, and "*" matches any existing entity.
func matchesETag(header, etag string) bool {
	if header == "" {
		return false
	}
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
		// A cache may revalidate with the weak form of a tag it was given.
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
