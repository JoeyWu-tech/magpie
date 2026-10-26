// Package jsmod compiles a plugin's JavaScript module for moejs, the
// JavaScript engine written in Go that magpie runs plugin code in its own
// process with: gateway middleware (internal/middleware) and agents
// (internal/agentplug).
package jsmod

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Calcium-Ion/moejs"
)

// Compile compiles the module in file and the modules it imports, which
// are files under root: there is no node_modules here, so a package is
// bundled into the module. what names the kind of module in an error
// ("a middleware"). It also gives the files it imported, one it couldn't
// read or compile among them, so that a caller keeping the module can
// tell when one changed (#3723). file's own time and size are the
// caller's to keep.
func Compile(root, file, what string) (*moejs.Module, Imports, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, err
	}
	entry, err := moejs.Compile(file, string(src))
	if err != nil {
		return nil, nil, err
	}
	var imports Imports
	mods := map[string]*moejs.Module{file: entry}
	mod, err := moejs.Link(entry, func(ref moejs.Referrer, spec string) (*moejs.Module, error) {
		if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
			return nil, fmt.Errorf("%q: %s imports only files beside it; bundle packages into it", spec, what)
		}
		from := file
		if ref != nil {
			from = ref.Name()
		}
		p := filepath.Clean(filepath.Join(filepath.Dir(from), filepath.FromSlash(spec)))
		if rel, err := filepath.Rel(root, p); err != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("%q is outside the plugin", spec)
		}
		if m, ok := mods[p]; ok {
			return m, nil
		}
		// looked at before it is read: a write after the look is a change
		imports = append(imports, look(p))
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		m, err := moejs.Compile(p, string(src))
		if err != nil {
			return nil, err
		}
		mods[p] = m
		return m, nil
	})
	return mod, imports, err
}

// Imports are the files a module imported, each with its time and size as
// they were when it was read.
type Imports []imported

type imported struct {
	path      string
	mod, size int64
	there     bool
}

// look is the file at path as it is now.
func look(path string) imported {
	fi, err := os.Stat(path)
	if err != nil {
		return imported{path: path}
	}
	return imported{path: path, mod: fi.ModTime().UnixNano(), size: fi.Size(), there: true}
}

// Changed says one of the files was written, made or removed since it was
// read.
func (is Imports) Changed() bool {
	for _, i := range is {
		if look(i.path) != i {
			return true
		}
	}
	return false
}
