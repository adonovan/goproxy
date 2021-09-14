// gomodproxy is a simple reference implementation of the core of a Go
// module proxy (https://golang.org/ref/mod), for pedagogical purposes.
// Each HTTP request is handled by directly executing the 'go' command.
//
// A realistic implementation would offer additional features, such as:
//
// - Caching, so that sequential requests for the same module do not
//   necessarily result in repeated execution of the go command.
// - Duplicate suppression, so that concurrent requests for the same
//   module do not result in duplicate work.
// - Replication and load balancing, so that the server can be run on
//   multiple hosts sharing persistent storage.
// - Cache eviction, to prevent unbounded growth of storage.
// - A checksum database, to avoid the need for "trust on first use".
// - Transport-layer security, to prevent eavesdropping in the network.
// - Authentication, so that only permitted users are served.
// - Access control, so that authenticated users may only read permitted packages.
// - Persistent storage, so that deletion or temporary downtime of a
//   repository does not break existing clients.
// - A content-delivery network, so that large .zip files can be
//   served from caches closer in the network to the requesting user.
// - Monitoring, logging, tracing, profiling, and other observability
//   features for maintainers.
//
// Examples of production-grade proxies are:
// - The Go Module Mirror, https://proxy.golang.org/
// - The Athens Project,  https://docs.gomods.io/
// - GoFig, https://gofig.dev/
//
//
// The Go module proxy protocol (golang.org/ref/mod#goproxy-protocol) defines five endpoints:
//
// - MODULE/@v/VERSION.info
// - MODULE/@v/VERSION.mod
// - MODULE/@v/VERSION.zip
//
//   These three endpoints accept version query (such as a semver or
//   branch name), and are implemented by a 'go mod download' command,
//   which resolves the version query, downloads the content of the
//   module from its version-control system (VCS) repository, and
//   saves its content (.zip, .mod) and metadata (.info) in separate
//   files in the cache directory.
//
//   Although the client could extract the .mod file from the .zip
//   file, it is more efficient to request the .mod file alone during
//   the initial "minimum version selection" phase and then request
//   the complete .zip later only if needed.
//
//   The results of these requests may be cached indefinitely, using
//   the pair (module, resolved version) as the key.  The 'go mod
//   download' command effectively does this for us, storing previous
//   results in its cache directory.
//
// - MODULE/@v/list
// - MODULE/@latest (optional)
//
//   These two endpoints request information about the available
//   versions of a module, and are implemented by 'go list -m'
//   commands: /@v/list uses -versions to query the tags in the
//   version-control system that hosts the module, and /@latest uses
//   the query "module@latest" to obtain the current version.
//
//   Because the set of versions may change at any moment, caching the
//   results of these queries inevitably results in the delivery of
//   stale information to some users at least some of the time.
//
//
// To use this proxy:
//
//    $ go run . &
//    $ export GOPROXY=http://localhost:8000/mod
//    $ go get <module>
//
package main

// TODO: when should we emit StatusGone? (see github.com/golang/go/issues/30134)

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/module"
)

var cachedir = filepath.Join(os.Getenv("HOME"), "gomodproxy-cache")

func main() {
	if err := os.MkdirAll(cachedir, 0755); err != nil {
		log.Fatalf("creating cache: %v", err)
	}
	http.HandleFunc("/mod/", handleMod)
	log.Fatal(http.ListenAndServe(":8000", nil))
}

func handleMod(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/mod/")

	// MODULE/@v/list
	if mod, ok := suffixed(path, "/@v/list"); ok {
		mod, err := module.UnescapePath(mod)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		log.Println("list", mod)

		versions, err := listVersions(mod)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		for _, v := range versions {
			fmt.Fprintln(w, v)
		}
		return
	}

	// MODULE/@latest
	if mod, ok := suffixed(path, "/@latest"); ok {
		mod, err := module.UnescapePath(mod)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		log.Println("latest", mod)

		latest, err := resolve(mod, "latest")
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		info := InfoJSON{Version: latest.Version, Time: latest.Time}
		json.NewEncoder(w).Encode(info)
		return
	}

	// MODULE/@v/VERSION.{info,mod,zip}
	if rest, ext, ok := lastCut(path, "."); ok && isOneOf(ext, "mod", "info", "zip") {
		if mod, version, ok := cut(rest, "/@v/"); ok {
			mod, err := module.UnescapePath(mod)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			version, err := module.UnescapeVersion(version)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			log.Printf("%s %s@%s", ext, mod, version)

			m, err := download(mod, version)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}

			// The version may be a query such as a branch name.
			// Branches move, so we suppress HTTP caching in that case.
			// (To avoid repeated calls to download, the proxy could use
			// the module name and resolved m.Version as a key in a cache.)
			if version != m.Version {
				w.Header().Set("Cache-Control", "no-store")
				log.Printf("%s %s@%s => %s", ext, mod, version, m.Version)
			}

			// Return the relevant cached file.
			var filename, mimetype string
			switch ext {
			case "info":
				filename = m.Info
				mimetype = "application/json"
			case "mod":
				filename = m.GoMod
				mimetype = "text/plain; charset=UTF-8"
			case "zip":
				filename = m.Zip
				mimetype = "application/zip"
			}
			w.Header().Set("Content-Type", mimetype)
			if err := copyFile(w, filename); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			return
		}
	}

	http.Error(w, "bad request", http.StatusBadRequest)
}

// download runs 'go mod download' and returns information about a
// specific module version. It also downloads the module's dependencies.
func download(name, version string) (*ModuleDownloadJSON, error) {
	var mod ModuleDownloadJSON
	if err := runGo(&mod, "mod", "download", "-json", name+"@"+version); err != nil {
		return nil, err
	}
	if mod.Error != "" {
		return nil, fmt.Errorf("failed to download module %s: %v", name, mod.Error)
	}
	return &mod, nil
}

// listVersions runs 'go list -m -versions' and returns an unordered list
// of versions of the specified module.
func listVersions(name string) ([]string, error) {
	var mod ModuleListJSON
	if err := runGo(&mod, "list", "-m", "-json", "-versions", name); err != nil {
		return nil, err
	}
	if mod.Error != nil {
		return nil, fmt.Errorf("failed to list module %s: %v", name, mod.Error.Err)
	}
	return mod.Versions, nil
}

// resolve runs 'go list -m' to resolve a module version query to a specific version.
func resolve(name, query string) (*ModuleListJSON, error) {
	var mod ModuleListJSON
	if err := runGo(&mod, "list", "-m", "-json", name+"@"+query); err != nil {
		return nil, err
	}
	if mod.Error != nil {
		return nil, fmt.Errorf("failed to list module %s: %v", name, mod.Error.Err)
	}
	return &mod, nil
}

// runGo runs the Go command and decodes its JSON output into result.
func runGo(result interface{}, args ...string) error {
	tmpdir, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	cmd := exec.Command("go", args...)
	cmd.Dir = tmpdir
	// Construct environment from scratch, for hygiene.
	cmd.Env = []string{
		"USER=" + os.Getenv("USER"),
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"NETRC=", // don't allow go command to read user's secrets
		"GOPROXY=direct",
		"GOCACHE=" + cachedir,
		"GOMODCACHE=" + cachedir,
		"GOSUMDB=",
	}
	cmd.Stdout = new(bytes.Buffer)
	cmd.Stderr = new(bytes.Buffer)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %v (stderr=<<%s>>)", cmd, err, cmd.Stderr)
	}
	if err := json.Unmarshal(cmd.Stdout.(*bytes.Buffer).Bytes(), result); err != nil {
		return fmt.Errorf("internal error decoding %s JSON output: %v", cmd, err)
	}
	return nil
}

// -- JSON schemas --

// ModuleDownloadJSON is the JSON schema of the output of 'go help mod download'.
type ModuleDownloadJSON struct {
	Path     string // module path
	Version  string // module version
	Error    string // error loading module
	Info     string // absolute path to cached .info file
	GoMod    string // absolute path to cached .mod file
	Zip      string // absolute path to cached .zip file
	Dir      string // absolute path to cached source root directory
	Sum      string // checksum for path, version (as in go.sum)
	GoModSum string // checksum for go.mod (as in go.sum)
}

// ModuleListJSON is the JSON schema of the output of 'go help list'.
type ModuleListJSON struct {
	Path      string          // module path
	Version   string          // module version
	Versions  []string        // available module versions (with -versions)
	Replace   *ModuleListJSON // replaced by this module
	Time      *time.Time      // time version was created
	Update    *ModuleListJSON // available update, if any (with -u)
	Main      bool            // is this the main module?
	Indirect  bool            // is this module only an indirect dependency of main module?
	Dir       string          // directory holding files for this module, if any
	GoMod     string          // path to go.mod file used when loading this module, if any
	GoVersion string          // go version used in module
	Retracted string          // retraction information, if any (with -retracted or -u)
	Error     *ModuleError    // error loading module
}

type ModuleError struct {
	Err string // the error itself
}

// InfoJSON is the JSON schema of the .info and @latest endpoints.
type InfoJSON struct {
	Version string
	Time    *time.Time
}

// -- helpers --

// suffixed reports whether x has the specified suffix,
// and returns the prefix.
func suffixed(x, suffix string) (rest string, ok bool) {
	if y := strings.TrimSuffix(x, suffix); y != x {
		return y, true
	}
	return
}

// See https://github.com/golang/go/issues/46336
func cut(s, sep string) (before, after string, found bool) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

func lastCut(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

// copyFile writes the content of the named file to dest.
func copyFile(dest io.Writer, name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(dest, f)
	return err
}

func isOneOf(s string, items ...string) bool {
	for _, item := range items {
		if s == item {
			return true
		}
	}
	return false
}
