package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/plugin"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

// startPluginTestServer：带插件注册表的测试服务端（一个捆绑插件 demo@1.2.3）。
func startPluginTestServer(t *testing.T) (*testServer, string /*pluginDir*/) {
	t.Helper()

	bundled := t.TempDir()
	agentDir := t.TempDir()
	dir := filepath.Join(bundled, "demo")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"id":"demo","name":"Demo","version":"1.2.3","minAppVersion":"0.1.0","main":"main.js"}`
	for path, content := range map[string]string{
		"manifest.json": manifest,
		"main.js":       "export default 1",
		"sub/x.txt":     "deep",
	} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := plugin.NewRegistry(bundled, agentDir)
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{
		opts: Options{
			Token:   "tok-test",
			Repo:    session.NewJsonlSessionRepo(t.TempDir()),
			Plugins: registry,
		},
		sessions: map[string]*session.Session{},
		metadata: map[string]session.Metadata{},
	}
	mux := http.NewServeMux()
	srv.routes(mux)
	return &testServer{handler: srv.middleware(mux)}, dir
}

func TestPluginsListAndToggle(t *testing.T) {
	harness, _ := startPluginTestServer(t)

	var infos []plugin.PluginInfo
	rec := doJSON(t, harness, "GET", "/v1/plugins", nil, &infos)
	if rec.Code != http.StatusOK || len(infos) != 1 || infos[0].ID != "demo" || !infos[0].Enabled {
		t.Fatalf("list: %d %+v", rec.Code, infos)
	}

	var ok protocol.OKResponse
	rec = doJSON(t, harness, "POST", "/v1/plugins/demo/enabled", map[string]bool{"enabled": false}, &ok)
	if rec.Code != http.StatusOK || !ok.OK {
		t.Fatalf("disable: %d %+v", rec.Code, ok)
	}
	doJSON(t, harness, "GET", "/v1/plugins", nil, &infos)
	if infos[0].Enabled {
		t.Fatal("disable did not stick")
	}

	if code := rawStatus(harness, "POST", "/v1/plugins/nope/enabled", `{"enabled":true}`); code != http.StatusNotFound {
		t.Fatalf("unknown plugin: %d", code)
	}
}

func rawStatus(harness *testServer, method, target, body string) int {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Authorization", "Bearer tok-test")
	rec := httptest.NewRecorder()
	harness.handler.ServeHTTP(rec, req)
	return rec.Code
}

func TestPluginStaticAssetServing(t *testing.T) {
	harness, _ := startPluginTestServer(t)

	cases := []struct {
		name   string
		target string
		want   int
		body   string
	}{
		{"happy", "/plugins/demo/main.js?v=1.2.3", http.StatusOK, "export default 1"},
		{"nested", "/plugins/demo/sub/x.txt?v=1.2.3", http.StatusOK, "deep"},
		{"version mismatch", "/plugins/demo/main.js?v=9.9.9", http.StatusNotFound, ""},
		{"missing v", "/plugins/demo/main.js", http.StatusNotFound, ""},
		{"unknown plugin", "/plugins/nope/main.js?v=1.2.3", http.StatusNotFound, ""},
		{"missing file", "/plugins/demo/nope.js?v=1.2.3", http.StatusNotFound, ""},
		{"traversal", "/plugins/demo/sub/../../../../etc/passwd?v=1.2.3", http.StatusNotFound, ""},
		{"manifest itself is reachable", "/plugins/demo/manifest.json?v=1.2.3", http.StatusOK, ""},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.target, nil)
		req.Header.Set("Authorization", "Bearer tok-test")
		rec := httptest.NewRecorder()
		harness.handler.ServeHTTP(rec, req)
		// 穿越路径会被 mux 或清洗层拦下：404/400 皆为拒绝，不许 200。
		if c.name == "traversal" {
			if rec.Code == http.StatusOK {
				t.Fatalf("%s: traversal served! body=%q", c.name, rec.Body.String())
			}
			continue
		}
		if rec.Code != c.want {
			t.Fatalf("%s: code=%d want=%d body=%s", c.name, rec.Code, c.want, rec.Body.String())
		}
		if c.body != "" && rec.Body.String() != c.body {
			t.Fatalf("%s: body=%q", c.name, rec.Body.String())
		}
	}

	// 静态端点公开：浏览器 import() 无法附加 Authorization header。
	req := httptest.NewRequest("GET", "/plugins/demo/main.js?v=1.2.3", nil)
	rec := httptest.NewRecorder()
	harness.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-token static: %d", rec.Code)
	}
}

func TestPluginRoutesWithoutRegistry(t *testing.T) {
	harness, _, _, _ := startTestServer(t) // 无 Plugins 配置
	if code := rawStatus(harness, "GET", "/v1/plugins", ""); code != http.StatusNotFound {
		t.Fatalf("nil registry: %d", code)
	}
}
