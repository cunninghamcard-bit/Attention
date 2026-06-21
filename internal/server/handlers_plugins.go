package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/plugin"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

type pluginEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	registry, ok := s.pluginRegistry(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, registry.List())
}

func (s *Server) handleSetPluginEnabled(w http.ResponseWriter, r *http.Request) {
	registry, ok := s.pluginRegistry(w)
	if !ok {
		return
	}

	var req pluginEnabledRequest
	if !decodeRequest(w, r, &req) {
		return
	}

	if err := registry.SetEnabled(r.PathValue("id"), req.Enabled); err != nil {
		if errors.Is(err, plugin.ErrPluginNotFound) {
			writeError(w, http.StatusNotFound, "plugin_not_found", "plugin not found")
			return
		}
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.OKResponse{OK: true})
}

// Decision: static plugin assets require Bearer token; renderer fetches with token. Revisit for public CDN use-case.
func (s *Server) handleStaticPluginAsset(w http.ResponseWriter, r *http.Request) {
	registry, ok := s.pluginRegistry(w)
	if !ok {
		return
	}

	info, ok := registry.Get(r.PathValue("id"))
	if !ok || info.LoadError != "" {
		writeError(w, http.StatusNotFound, "not_found", "plugin asset not found")
		return
	}
	if r.URL.Query().Get("v") != info.Version {
		writeError(w, http.StatusNotFound, "not_found", "plugin asset not found")
		return
	}

	cleanAsset, err := cleanPluginAssetPath(r.PathValue("asset"), info.Dir)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	root, err := os.OpenRoot(info.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", "plugin asset not found")
			return
		}
		writeMappedError(w, err)
		return
	}
	defer root.Close()

	fileInfo, status, err := statPluginAsset(root, cleanAsset)
	if err != nil {
		switch status {
		case http.StatusBadRequest:
			writeError(w, status, "bad_request", err.Error())
			return
		case http.StatusInternalServerError:
			writeMappedError(w, err)
			return
		default:
			writeError(w, status, "not_found", "plugin asset not found")
			return
		}
	}

	file, err := root.Open(cleanAsset)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", "plugin asset not found")
			return
		}
		writeMappedError(w, err)
		return
	}
	defer file.Close()

	http.ServeContent(w, r, filepath.Base(cleanAsset), fileInfo.ModTime(), file)
}

func (s *Server) pluginRegistry(w http.ResponseWriter) (*plugin.Registry, bool) {
	if s.opts.Plugins == nil {
		writeError(w, http.StatusNotFound, "plugins_unavailable", "plugin registry not configured")
		return nil, false
	}
	return s.opts.Plugins, true
}

func cleanPluginAssetPath(raw string, pluginDir string) (string, error) {
	if raw == "" {
		return "", errors.New("plugin asset path: required")
	}
	if strings.Contains(raw, "\x00") || strings.Contains(raw, `\`) {
		return "", errors.New("plugin asset path: invalid path")
	}

	clean := filepath.Clean(filepath.FromSlash(raw))
	if clean == "." || !filepath.IsLocal(clean) {
		return "", errors.New("plugin asset path: escapes plugin directory")
	}

	full := filepath.Join(pluginDir, clean)
	rel, err := filepath.Rel(pluginDir, full)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("plugin asset path: escapes plugin directory")
	}
	return clean, nil
}

func statPluginAsset(root *os.Root, cleanAsset string) (os.FileInfo, int, error) {
	parts := strings.Split(filepath.ToSlash(cleanAsset), "/")
	current := ""
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, http.StatusNotFound, err
			}
			return nil, http.StatusInternalServerError, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, http.StatusNotFound, errors.New("plugin asset path: symlinks are not served")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, http.StatusNotFound, errors.New("plugin asset path: parent is not a directory")
		}
		if i == len(parts)-1 {
			if !info.Mode().IsRegular() {
				return nil, http.StatusNotFound, errors.New("plugin asset path: not a regular file")
			}
			return info, http.StatusOK, nil
		}
	}
	return nil, http.StatusBadRequest, errors.New("plugin asset path: required")
}
