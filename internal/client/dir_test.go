package client

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestServeDir(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"index.html": "app shell", "app.js": "js"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	addr, stop, err := ServeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	get := func(path string) (int, string) {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for path, want := range map[string]string{
		"/app.js":         "js",
		"/":               "app shell",
		"/settings/users": "app shell", // client-side route
	} {
		if code, body := get(path); code != 200 || body != want {
			t.Errorf("%s: %d %q", path, code, body)
		}
	}
	if code, _ := get("/missing.png"); code != http.StatusNotFound {
		t.Errorf("missing file: %d", code)
	}
	if code, _ := get("/../../etc/passwd"); code == 200 {
		t.Error("path traversal served a file")
	}
	if _, _, err := ServeDir(filepath.Join(dir, "app.js")); err == nil {
		t.Error("a file was accepted as a directory")
	}
}
