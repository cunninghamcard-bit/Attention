// Package server 是控制面：REST+SSE 协议服务 + 资源 CRUD。执行面只经三接口可达（D19）。
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/plugin"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

type Options struct {
	Addr        string // "127.0.0.1:0" = 随机端口
	Token       string
	FrontendDir string
	DefaultCWD  string // create 未带 cwd 时的会话工作目录（引擎进程 cwd）
	Store       backend.EventStore
	Bus         backend.NotifyBus
	Queue       backend.JobQueue
	Repo        session.JsonlSessionRepoAPI
	Plugins     *plugin.Registry
	ExtCommands ExtCommandDispatcher
	Logger      *slog.Logger
}

type Server struct {
	opts Options
	ln   net.Listener
	http *http.Server

	mu       sync.Mutex
	sessions map[string]*session.Session
	metadata map[string]session.Metadata
}

func Start(ctx context.Context, opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	if opts.Store == nil {
		return nil, fmt.Errorf("server: event store is required")
	}
	if opts.Bus == nil {
		return nil, fmt.Errorf("server: notify bus is required")
	}
	if opts.Queue == nil {
		return nil, fmt.Errorf("server: job queue is required")
	}
	if opts.Repo == nil {
		return nil, fmt.Errorf("server: session repo is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", opts.Addr)
	if err != nil {
		return nil, err
	}

	srv := &Server{
		opts:     opts,
		ln:       ln,
		sessions: map[string]*session.Session{},
		metadata: map[string]session.Metadata{},
	}

	mux := http.NewServeMux()
	srv.routes(mux)
	srv.http = &http.Server{Handler: srv.middleware(mux)}

	go func() {
		if err := srv.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			opts.Logger.Error("server stopped", "error", err)
		}
	}()

	return srv, nil
}

func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

func (s *Server) Close() error {
	return s.http.Close()
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /v1/sessions/{id}/fork", s.handleFork)
	mux.HandleFunc("POST /v1/sessions/{id}/prompt", s.handlePrompt)
	mux.HandleFunc("POST /v1/sessions/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /v1/sessions/{id}/ui-resolve", s.handleUIResolve)
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.handleEventsNoBuffer)
	mux.HandleFunc("GET /v1/plugins", s.handleListPlugins)
	mux.HandleFunc("POST /v1/plugins/{id}/enabled", s.handleSetPluginEnabled)
	mux.HandleFunc("POST /v1/ext/command", s.handleExtCommand)
	mux.HandleFunc("GET /plugins/{id}/{asset...}", s.handleStaticPluginAsset)
	if s.opts.FrontendDir != "" {
		mux.HandleFunc("GET /{asset...}", s.handleFrontendAsset)
	}
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Along-Schema", protocol.SchemaVersion)
		if isAPIPath(r.URL.Path) && !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		if got := r.Header.Get("X-Along-Schema"); got != "" && got != protocol.SchemaVersion {
			writeError(w, http.StatusBadRequest, "schema_mismatch", "unsupported schema version")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleEventsNoBuffer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Accel-Buffering", "no")
	s.handleEvents(w, r)
}

func (s *Server) handleFrontendAsset(w http.ResponseWriter, r *http.Request) {
	if isAPIPath(r.URL.Path) || isPluginStaticPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}

	root, err := os.OpenRoot(s.opts.FrontendDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", "frontend asset not found")
			return
		}
		writeMappedError(w, err)
		return
	}
	defer root.Close()

	rawAsset := strings.TrimPrefix(r.URL.Path, "/")
	if rawAsset == "" {
		rawAsset = "index.html"
	}
	served, status, err := serveFrontendFile(w, r, root, rawAsset)
	if err == nil && served {
		return
	}
	if status == http.StatusNotFound {
		served, status, err = serveFrontendFile(w, r, root, "index.html")
		if err == nil && served {
			return
		}
	}
	writeFrontendError(w, status, err)
}

func (s *Server) authorized(r *http.Request) bool {
	if s.opts.Token == "" {
		return true
	}
	return strings.TrimSpace(r.Header.Get("Authorization")) == "Bearer "+s.opts.Token
}

func isAPIPath(path string) bool {
	return path == "/v1" || strings.HasPrefix(path, "/v1/")
}

func isPluginStaticPath(path string) bool {
	return path == "/plugins" || strings.HasPrefix(path, "/plugins/")
}

func serveFrontendFile(
	w http.ResponseWriter,
	r *http.Request,
	root *os.Root,
	rawAsset string,
) (bool, int, error) {
	cleanAsset, err := cleanFrontendAssetPath(rawAsset)
	if err != nil {
		return false, http.StatusBadRequest, err
	}

	fileInfo, status, err := statFrontendAsset(root, cleanAsset)
	if err != nil {
		return false, status, err
	}

	file, err := root.Open(cleanAsset)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, http.StatusNotFound, err
		}
		return false, http.StatusInternalServerError, err
	}
	defer file.Close()

	http.ServeContent(w, r, filepath.Base(cleanAsset), fileInfo.ModTime(), file)
	return true, http.StatusOK, nil
}

func cleanFrontendAssetPath(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("frontend asset path: required")
	}
	if strings.Contains(raw, "\x00") || strings.Contains(raw, `\`) {
		return "", errors.New("frontend asset path: invalid path")
	}

	clean := filepath.Clean(filepath.FromSlash(raw))
	if clean == "." || !filepath.IsLocal(clean) {
		return "", errors.New("frontend asset path: escapes frontend directory")
	}
	return clean, nil
}

func statFrontendAsset(root *os.Root, cleanAsset string) (os.FileInfo, int, error) {
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
			return nil, http.StatusNotFound, errors.New("frontend asset path: symlinks are not served")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, http.StatusNotFound, errors.New("frontend asset path: parent is not a directory")
		}
		if i == len(parts)-1 {
			if !info.Mode().IsRegular() {
				return nil, http.StatusNotFound, errors.New("frontend asset path: not a regular file")
			}
			return info, http.StatusOK, nil
		}
	}
	return nil, http.StatusBadRequest, errors.New("frontend asset path: required")
}

func writeFrontendError(w http.ResponseWriter, status int, err error) {
	if status == http.StatusBadRequest {
		writeError(w, status, "bad_request", err.Error())
		return
	}
	if status == http.StatusInternalServerError {
		writeMappedError(w, err)
		return
	}
	writeError(w, status, "not_found", "frontend asset not found")
}
