package servers

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
	"github.com/google/uuid"
)

// A generated image arrives in the answer as a markdown link to a designerapp
// address, which nothing outside this process can fetch: the download needs the
// designer access token in Authorization plus the fileToken as its own header,
// and an <img> element sends neither.
//
// So the address is replaced with a route on this gateway before the answer
// goes out, and the gateway performs the download when that route is called.
// The route takes a reference this process minted, never an address the caller
// chose, because a route that fetched a caller-supplied address would be an
// SSRF surface even behind the host allowlist.

const (
	// generatedImagePrefix is the route the rewritten markdown points at. It is
	// root-relative, so no handler has to trust the Host header to build it.
	generatedImagePrefix = "/v1/images/"
	// generatedImageTTL bounds how long a reference stays resolvable. The
	// fileToken inside the address expires on its own schedule, so a longer
	// life would only keep dead references.
	generatedImageTTL = 12 * time.Hour
	// generatedImageMaxRefs caps the stored references. The oldest goes first,
	// which costs a conversation its earliest images rather than the process
	// its memory.
	generatedImageMaxRefs = 512
)

// imageRef is one stored address and the moment it stops resolving.
type imageRef struct {
	url       string
	expiresAt time.Time
}

// imageRefStore maps a reference this process minted to the address it stands
// for. It holds no caller-supplied address: every entry passed
// validateImageDownloadURL on the way in.
type imageRefStore struct {
	mu      sync.Mutex
	entries map[string]imageRef
	// order records insertion order so the oldest entry can be evicted without
	// scanning the map for a minimum.
	order []string
	now   func() time.Time
}

// newImageRefStore creates an empty store.
func newImageRefStore() *imageRefStore {
	return &imageRefStore{
		entries: map[string]imageRef{},
		now:     time.Now,
	}
}

// put stores an address and returns its reference.
func (s *imageRefStore) put(url string) string {
	id := uuid.NewString()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[id] = imageRef{url: url, expiresAt: s.now().Add(generatedImageTTL)}
	s.order = append(s.order, id)
	for len(s.order) > generatedImageMaxRefs {
		delete(s.entries, s.order[0])
		s.order = s.order[1:]
	}
	return id
}

// get resolves a reference. An expired entry is dropped and reported as absent.
func (s *imageRefStore) get(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.entries[id]
	if !ok {
		return "", false
	}
	if !s.now().Before(ref.expiresAt) {
		delete(s.entries, id)
		return "", false
	}
	return ref.url, true
}

// routeGeneratedImages rewrites every generated-image link in one piece of
// answer text to a route on this gateway.
//
// A link whose address this gateway refuses to download is removed rather than
// forwarded, for the reason buildOpenAIImageData drops one: handing a
// model-controlled address to a client makes the client fetch it.
//
// The image markdown reaches the server as a chunk of its own, so a link is
// never split across two calls.
func (api *APIServer) routeGeneratedImages(text string) string {
	if text == "" || !strings.Contains(text, "](http") {
		return text
	}
	return urlImagePattern.ReplaceAllStringFunc(text, func(match string) string {
		groups := urlImagePattern.FindStringSubmatch(match)
		if len(groups) < 2 {
			return match
		}
		url := groups[1]
		if err := api.validateImageDownloadURL(url); err != nil {
			logging.Errorf("routeGeneratedImages: dropping image link: %v", err)
			return ""
		}
		return "![image](" + generatedImagePrefix + api.imageRefs.put(url) + ")"
	})
}

// handleGeneratedImage serves an image the gateway generated, by the reference
// routeGeneratedImages minted for it.
func (api *APIServer) handleGeneratedImage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodGet {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	id := strings.TrimPrefix(r.URL.Path, generatedImagePrefix)
	if id == "" || strings.Contains(id, "/") {
		api.sendErrorCode(w, http.StatusNotFound, "image_not_found", "No image for this reference")
		return
	}

	url, ok := api.imageRefs.get(id)
	if !ok {
		// A reference is lost on restart and expires on its own, and the
		// address behind it expires too. The interface shows a placeholder
		// rather than a broken image.
		api.sendErrorCode(w, http.StatusNotFound, "image_not_found", "No image for this reference")
		return
	}

	body, contentType, err := api.downloadImage(url)
	if err != nil {
		logging.Errorf("handleGeneratedImage: download failed: %v", err)
		api.sendErrorCode(w, http.StatusBadGateway, "upstream_error", "The generated image could not be downloaded")
		return
	}

	// The bytes are in hand by now, so the validator costs one hash and saves
	// the whole body on a repeat request. It cannot save the download itself:
	// the tag is not known until the image has been fetched.
	etag := contentETag(body)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("ETag", etag)
	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		logging.Errorf("handleGeneratedImage: write failed: %v", err)
	}
}
