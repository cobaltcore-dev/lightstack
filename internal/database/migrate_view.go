package database

import (
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// One migrations folder for both backends (FR-024, research R10).
//
// golang-migrate refuses two files with the same version, so each backend reads the
// folder through a view that applies three rules:
//
//  1. A .postgres or .sqlite file is used only by that backend.
//  2. When a backend has its own file for a version, that file replaces the plain file
//     with the same number.
//  3. SQLite skips every version below its baseline (NNN_baseline.sqlite.up.sql).
//
// Down files follow the same rules. The view serves the kept files under their own
// names, so golang-migrate parses them exactly as it would the folder.

var migrationFileName = regexp.MustCompile(`^([0-9]+)_(.+)\.(up|down)\.sql$`)

type migrationFile struct {
	name      string
	version   uint64
	ident     string
	direction string
	backend   Backend // "" for a plain file
}

func parseMigrationFile(name string) (migrationFile, bool) {
	m := migrationFileName.FindStringSubmatch(name)
	if m == nil {
		return migrationFile{}, false
	}
	v, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return migrationFile{}, false
	}
	f := migrationFile{name: name, version: v, ident: m[2], direction: m[3]}
	switch {
	case strings.HasSuffix(f.ident, ".postgres"):
		f.backend, f.ident = Postgres, strings.TrimSuffix(f.ident, ".postgres")
	case strings.HasSuffix(f.ident, ".sqlite"):
		f.backend, f.ident = SQLite, strings.TrimSuffix(f.ident, ".sqlite")
	}
	return f, true
}

// baselineVersion is the version of NNN_baseline.sqlite.up.sql, where SQLite starts. The
// folder must hold exactly one.
func baselineVersion(src fs.FS) (uint64, error) {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return 0, fmt.Errorf("read migrations: %w", err)
	}
	var found []uint64
	for _, e := range entries {
		if f, ok := parseMigrationFile(e.Name()); ok && f.backend == SQLite && f.ident == "baseline" && f.direction == "up" {
			found = append(found, f.version)
		}
	}
	switch len(found) {
	case 0:
		return 0, fmt.Errorf("no SQLite baseline (NNN_baseline.sqlite.up.sql) in the migrations folder")
	case 1:
		return found[0], nil
	}
	return 0, fmt.Errorf("two SQLite baselines in the migrations folder: versions %d and %d", found[0], found[1])
}

type migrationView struct {
	src     fs.FS
	entries []fs.DirEntry
	kept    map[string]bool
}

func newMigrationView(src fs.FS, backend Backend) (*migrationView, error) {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var baseline uint64
	if backend == SQLite {
		if baseline, err = baselineVersion(src); err != nil {
			return nil, err
		}
	}
	kept, err := selectFiles(entries, backend, baseline)
	if err != nil {
		return nil, err
	}
	v := &migrationView{src: src, kept: map[string]bool{}}
	for name, e := range kept {
		v.kept[name] = true
		v.entries = append(v.entries, e)
	}
	sort.Slice(v.entries, func(i, j int) bool { return v.entries[i].Name() < v.entries[j].Name() })
	return v, nil
}

// selectFiles applies the three view rules to entries and returns the kept files by name: files of
// the other backend are dropped (rule 1), a backend's own file replaces the plain file of its
// number (rule 2), and SQLite skips versions below its baseline (rule 3). Two files left for one
// version and direction is an error.
func selectFiles(entries []fs.DirEntry, backend Backend, baseline uint64) (map[string]fs.DirEntry, error) {
	type key struct {
		version   uint64
		direction string
	}
	chosen := map[key]migrationFile{}
	byName := map[string]fs.DirEntry{}
	for _, e := range entries {
		f, ok := parseMigrationFile(e.Name())
		if !ok || e.IsDir() {
			continue
		}
		if f.backend != "" && f.backend != backend {
			continue // rule 1
		}
		if backend == SQLite && f.version < baseline {
			continue // rule 3
		}
		k := key{f.version, f.direction}
		prev, dup := chosen[k]
		switch {
		case !dup:
			chosen[k] = f
		case prev.backend == "" && f.backend == backend:
			chosen[k] = f // rule 2: the backend's own file replaces the plain one
		case prev.backend == backend && f.backend == "":
			// rule 2, the other way round: keep the backend's own file
		default:
			return nil, fmt.Errorf("two %s migrations for version %d in the %s view: %s and %s",
				f.direction, f.version, backend, prev.name, f.name)
		}
		byName[e.Name()] = e
	}
	kept := map[string]fs.DirEntry{}
	for _, f := range chosen {
		kept[f.name] = byName[f.name]
	}
	return kept, nil
}

// Open serves a kept file; every other name does not exist in the view.
func (v *migrationView) Open(name string) (fs.File, error) {
	if name == "." || v.kept[name] {
		return v.src.Open(name)
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// latestVersion is the newest migration version the view holds.
func (v *migrationView) latestVersion() uint64 {
	var latest uint64
	for _, e := range v.entries {
		if f, ok := parseMigrationFile(e.Name()); ok && f.version > latest {
			latest = f.version
		}
	}
	return latest
}

// ReadDir lists the kept files.
func (v *migrationView) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	return append([]fs.DirEntry(nil), v.entries...), nil
}

// validateView checks that every up migration of the view has its down migration and
// the reverse (FR-024). A backend's own up file needs a down file of its own: the plain
// down file of that number undoes the plain up file, not this one. The reverse is
// allowed — a backend's own down file beside the plain up file corrects how that backend
// rolls the plain file back.
func validateView(v *migrationView, backend Backend) (int, error) {
	ups, downs := map[uint64]migrationFile{}, map[uint64]migrationFile{}
	for _, e := range v.entries {
		f, _ := parseMigrationFile(e.Name())
		if f.direction == "up" {
			ups[f.version] = f
		} else {
			downs[f.version] = f
		}
	}
	for ver, up := range ups {
		down, ok := downs[ver]
		if !ok {
			return 0, fmt.Errorf("%s view: %s has no down migration", backend, up.name)
		}
		if up.backend != "" && down.backend == "" {
			return 0, fmt.Errorf("%s view: %s has no down migration of its own (%s is the plain file's)", backend, up.name, down.name)
		}
	}
	for ver, down := range downs {
		if _, ok := ups[ver]; !ok {
			return 0, fmt.Errorf("%s view: %s has no up migration", backend, down.name)
		}
	}
	return len(ups), nil
}
