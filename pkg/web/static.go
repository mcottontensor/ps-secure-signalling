package web

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// Static serves the files under dir, and homepage at "/". Files are opened through os.Root, so
// neither ".." nor a symlink can reach outside dir. Directory listings and dotfiles are never
// served, and only GET and HEAD are accepted.
func Static(dir, homepage string) (http.Handler, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	fsys := noListing{root.FS()}
	if homepage == "" {
		homepage = "player.html"
	}
	if _, err := fs.Stat(fsys, homepage); err != nil {
		return nil, errors.New("web: homepage " + homepage + " not found in " + dir)
	}
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := path.Clean("/" + r.URL.Path)
		if strings.Contains(p, "/.") {
			http.NotFound(w, r)
			return
		}
		if p == "/" {
			http.ServeFileFS(w, r, fsys, homepage)
			return
		}
		files.ServeHTTP(w, r)
	}), nil
}

// noListing refuses to open directories that have no index.html, so the file server never
// generates a listing.
type noListing struct{ fs.FS }

func (n noListing) Open(name string) (fs.File, error) {
	f, err := n.FS.Open(name)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil && st.IsDir() {
		if _, err := fs.Stat(n.FS, path.Join(name, "index.html")); err != nil {
			f.Close()
			return nil, fs.ErrNotExist
		}
	}
	return f, nil
}
