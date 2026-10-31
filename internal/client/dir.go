package client

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ServeDir serves the files in dir on a loopback port and returns its address,
// so a tunnel can point at a folder (ts http ./dist). Requests for missing
// paths fall back to index.html when the folder has one (single-page apps).
func ServeDir(dir string) (addr string, stop func(), err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, err
	}
	if fi, err := os.Stat(abs); err != nil {
		return "", nil, err
	} else if !fi.IsDir() {
		return "", nil, errors.New(dir + " is not a directory")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: dirHandler(abs), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Close() }, nil
}

func dirHandler(root string) http.Handler {
	files := http.FileServer(http.Dir(root))
	index := filepath.Join(root, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(filepath.Clean("/"+r.URL.Path)))); errors.Is(err, os.ErrNotExist) {
			if _, err := os.Stat(index); err == nil && filepath.Ext(r.URL.Path) == "" {
				http.ServeFile(w, r, index)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
