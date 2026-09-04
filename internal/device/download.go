package device

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"robloxkit/internal/session"
)

// Header names carrying artifact integrity metadata.
const (
	ChecksumHeader = "X-Checksum-Sha256"
	VersionHeader  = "X-Bridge-Version"
)

// SessionValidator verifies opaque browser session tokens.
type SessionValidator interface {
	Validate(ctx context.Context, token string) (session.Session, error)
}

// Artifact describes the downloadable Bridge executable.
type Artifact struct {
	Version  string
	Filename string
	Path     string
}

type artifactInfo struct {
	Version   string `json:"version"`
	Filename  string `json:"filename"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// DownloadHandler streams the Bridge artifact to authenticated sessions.
// Download never gates on entitlement state and never starts the trial.
type DownloadHandler struct {
	sessions SessionValidator
	artifact Artifact
	checksum string
	size     int64
}

// NewDownloadHandler verifies the artifact is readable and caches its
// checksum and size so every response serves consistent integrity metadata.
func NewDownloadHandler(sessions SessionValidator, artifact Artifact) (*DownloadHandler, error) {
	if sessions == nil {
		return nil, errors.New("device: nil session validator")
	}
	info, err := inspectArtifact(artifact)
	if err != nil {
		return nil, err
	}
	return &DownloadHandler{sessions: sessions, artifact: artifact, checksum: info.SHA256, size: info.SizeBytes}, nil
}

// ServeHTTP enforces a web session and streams the artifact with checksum
// and version headers.
func (h *DownloadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := requireSession(r, h.sessions); err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	file, err := os.Open(h.artifact.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "artifact unavailable")
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set(ChecksumHeader, h.checksum)
	w.Header().Set(VersionHeader, h.artifact.Version)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", h.artifact.Filename))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", h.size))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

// DownloadMetadataHandler serves the artifact metadata the dashboard shows.
// It never exposes the artifact body or any token.
type DownloadMetadataHandler struct {
	sessions SessionValidator
	artifact Artifact
	checksum string
	size     int64
}

// NewDownloadMetadataHandler verifies the artifact and constructs the
// metadata handler.
func NewDownloadMetadataHandler(sessions SessionValidator, artifact Artifact) (*DownloadMetadataHandler, error) {
	if sessions == nil {
		return nil, errors.New("device: nil session validator")
	}
	info, err := inspectArtifact(artifact)
	if err != nil {
		return nil, err
	}
	return &DownloadMetadataHandler{sessions: sessions, artifact: artifact, checksum: info.SHA256, size: info.SizeBytes}, nil
}

// ServeHTTP enforces a web session and returns artifact metadata as JSON.
func (h *DownloadMetadataHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := requireSession(r, h.sessions); err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(artifactInfo{
		Version:   h.artifact.Version,
		Filename:  h.artifact.Filename,
		SHA256:    h.checksum,
		SizeBytes: h.size,
	})
}

func inspectArtifact(artifact Artifact) (artifactInfo, error) {
	if artifact.Version == "" || artifact.Filename == "" {
		return artifactInfo{}, errors.New("device: artifact version and filename are required")
	}
	file, err := os.Open(artifact.Path)
	if err != nil {
		return artifactInfo{}, fmt.Errorf("device: open artifact: %w", err)
	}
	defer file.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	if err != nil {
		return artifactInfo{}, fmt.Errorf("device: read artifact: %w", err)
	}
	if size == 0 {
		return artifactInfo{}, errors.New("device: artifact is empty")
	}
	return artifactInfo{
		Version:   artifact.Version,
		Filename:  artifact.Filename,
		SHA256:    hex.EncodeToString(hasher.Sum(nil)),
		SizeBytes: size,
	}, nil
}
