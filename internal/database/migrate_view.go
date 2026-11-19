package database

import (
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// One migrations folder for both backends (SPEC-028 FR-024, research R10).
//
// golang-migrate refuses two files with the same version, so each backend reads the
// folder through a view that applies two rules:
//
//  1. A .postgres or .sqlite file is used only by that backend.
//  2. When a backend has its own file for a version, that file replaces the plain file
//     with the same number.
//
// Down files follow the same rules. The view serves the kept files under their own
// names, so golang-migrate parses them exactly as it would the folder. Both backends start
// at the baseline, 001 (SPEC-032 FR-006): the rule that let SQLite skip the versions below
// its own later baseline is retired with the history it served.
//
// The view also serves the open release migration, NNN_open.up.sql and NNN_open.down.sql,
// assembled from the fragments in migrations/next/ (migrate_fragments.go). Every reader of the
// folder reads it through the view — startup, every lightstack-migrate command, the integrity
// check's reference schema and the tests — so each of them sees the fragments (FR-017).

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

type migrationView struct {
	src     fs.FS
	entries []fs.DirEntry
	kept    map[string]bool
	open    map[string][]byte // the open release migration's two files, served from memory
}

func newMigrationView(src fs.FS, backend Backend) (*migrationView, error) {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	kept, err := selectFiles(entries, backend)
	if err != nil {
		return nil, err
	}
	v := &migrationView{src: src, kept: map[string]bool{}, open: map[string][]byte{}}
	for name, e := range kept {
		v.kept[name] = true
		v.entries = append(v.entries, e)
	}
	open, err := assembleOpen(src, backend)
	if err != nil {
		return nil, err
	}
	if open != nil {
		v.open[fmt.Sprintf("%03d_open.up.sql", open.version)] = open.up
		v.open[fmt.Sprintf("%03d_open.down.sql", open.version)] = open.down
		for name, data := range v.open {
			v.entries = append(v.entries, memFileInfo{name: name, size: int64(len(data))})
		}
	}
	slices.SortFunc(v.entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return v, nil
}

// selectFiles applies the view rules to entries and returns the kept files by name: files of the
// other backend are dropped (rule 1), and a backend's own file replaces the plain file of its
// number (rule 2). Two files left for one version and direction is an error.
func selectFiles(entries []fs.DirEntry, backend Backend) (map[string]fs.DirEntry, error) {
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

// Open serves a kept file or one of the open release migration's; every other name does not exist
// in the view.
func (v *migrationView) Open(name string) (fs.File, error) {
	if data, ok := v.open[name]; ok {
		return &memFile{Reader: bytes.NewReader(data), info: memFileInfo{name: name, size: int64(len(data))}}, nil
	}
	if name == "." || v.kept[name] {
		return v.src.Open(name)
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// upFile is the view's up migration for version.
func (v *migrationView) upFile(version uint64) (migrationFile, bool) {
	for _, e := range v.entries {
		if f, ok := parseMigrationFile(e.Name()); ok && f.version == version && f.direction == "up" {
			return f, true
		}
	}
	return migrationFile{}, false
}

// kind says what version is in this view: none, the baseline, a release migration or the open
// release migration (FR-022). It is empty for a version the view does not hold.
func (v *migrationView) kind(version uint64) string {
	if version == 0 {
		return "none (empty database)"
	}
	f, ok := v.upFile(version)
	switch {
	case !ok:
		return ""
	case f.ident == "baseline":
		return "baseline"
	case f.ident == "open":
		return fmt.Sprintf("open release migration %03d (unreleased)", version)
	}
	return "release " + f.ident
}

// expectedChecksum is the checksum the view's up migration for version records in its header: what
// a database that applied that migration, as the view serves it now, holds in applied_migrations
// (FR-019, FR-020).
func (v *migrationView) expectedChecksum(version uint64) (string, error) {
	f, ok := v.upFile(version)
	if !ok {
		return "", fmt.Errorf("the migrations hold no up migration for version %03d", version)
	}
	data, err := fs.ReadFile(v, f.name)
	if err != nil {
		return "", err
	}
	sum, err := parseChecksumHeader(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", f.name, err)
	}
	return sum, nil
}

// staleness compares a database's record of version — the checksum applied_migrations holds for
// it, nil for none — with what the view serves for version now, and says why they disagree, or
// returns nil when they agree (FR-019, FR-020).
func (v *migrationView) staleness(version uint64, record *string) (*StaleError, error) {
	if record == nil {
		return &StaleError{Version: version, NoRecord: true}, nil
	}
	want, err := v.expectedChecksum(version)
	if err != nil || *record == want {
		return nil, err
	}
	stale := &StaleError{Version: version}
	switch f, _ := v.upFile(version); f.ident {
	case "open":
	case "baseline":
		stale.Baseline = true
	default:
		stale.Release = f.ident
	}
	return stale, nil
}

// memFile is a file of the open release migration, served from memory.
type memFile struct {
	*bytes.Reader
	info memFileInfo
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *memFile) Close() error               { return nil }

// memFileInfo describes a file served from memory, as a directory entry and as file info.
type memFileInfo struct {
	name string
	size int64
}

func (i memFileInfo) Name() string               { return i.name }
func (i memFileInfo) Size() int64                { return i.size }
func (i memFileInfo) Mode() fs.FileMode          { return 0o444 }
func (i memFileInfo) ModTime() time.Time         { return time.Time{} }
func (i memFileInfo) IsDir() bool                { return false }
func (i memFileInfo) Sys() any                   { return nil }
func (i memFileInfo) Type() fs.FileMode          { return 0 }
func (i memFileInfo) Info() (fs.FileInfo, error) { return i, nil }

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

// The folder grammar from the squash on (SPEC-032 contracts/migrations-folder.md). Version 001 is
// the baseline pair of each backend; every later numbered file is a release migration, frozen by
// the release step, numbered consecutively from 002 with one release per version.
var (
	baselineFiles = []string{
		"001_baseline.postgres.up.sql", "001_baseline.postgres.down.sql",
		"001_baseline.sqlite.up.sql", "001_baseline.sqlite.down.sql",
	}
	releaseFileName = regexp.MustCompile(`^(\d{3})_(v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)(?:\.(?:postgres|sqlite))?\.(?:up|down)\.sql$`)
)

// validateFolderNames checks the numbered files of the folder against the grammar. Any numbered
// file that is neither the baseline nor a release migration is refused: schema changes go into
// fragments (FR-026). Whether every up file has its down file is validateView's check.
func validateFolderNames(src fs.FS) error {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	present := map[string]bool{}
	releases := releaseIndex{releaseOf: map[uint64]string{}, versionOf: map[string]uint64{}, firstFile: map[uint64]string{}}
	for _, e := range entries { // fs.ReadDir sorts by name
		f, ok := parseMigrationFile(e.Name())
		switch {
		case !ok || e.IsDir():
		case f.version == 1:
			if !slices.Contains(baselineFiles, e.Name()) {
				return fmt.Errorf("version 001 must be the baseline pair of each backend (%s): %s is not one of them",
					strings.Join(baselineFiles, ", "), e.Name())
			}
			present[e.Name()] = true
		default:
			if err := releases.add(f.version, e.Name()); err != nil {
				return err
			}
		}
	}
	if err := checkBaselinePair(present); err != nil {
		return err
	}
	return releases.checkNumbering()
}

// checkBaselinePair refuses a folder whose baseline lacks the up file of either backend.
func checkBaselinePair(present map[string]bool) error {
	for _, up := range []string{"001_baseline.postgres.up.sql", "001_baseline.sqlite.up.sql"} {
		if !present[up] {
			return fmt.Errorf("version 001 must be the baseline pair of each backend (%s): %s is missing",
				strings.Join(baselineFiles, ", "), up)
		}
	}
	return nil
}

// releaseIndex maps the release migrations' versions to their releases and back, so a version naming
// two releases, a release on two versions and a gap in the numbering are refused.
type releaseIndex struct {
	releaseOf map[uint64]string // version → release
	versionOf map[string]uint64 // release → version
	firstFile map[uint64]string // version → its first file, named in errors
}

// add records the numbered file name at version, refusing one that is not a release migration.
func (r *releaseIndex) add(version uint64, name string) error {
	m := releaseFileName.FindStringSubmatch(name)
	if m == nil {
		return fmt.Errorf("%s: a numbered migration other than the baseline or a release migration; "+
			"schema changes go into fragments (migrations/next/)", name)
	}
	release := m[2]
	if prev, ok := r.releaseOf[version]; ok && prev != release {
		return fmt.Errorf("version %03d names two releases: %s and %s", version, prev, release)
	}
	if prev, ok := r.versionOf[release]; ok && prev != version {
		return fmt.Errorf("release %s has two versions: %03d and %03d", release, prev, version)
	}
	r.releaseOf[version], r.versionOf[release] = release, version
	if _, ok := r.firstFile[version]; !ok {
		r.firstFile[version] = name
	}
	return nil
}

// checkNumbering refuses release migrations not numbered consecutively from 002.
func (r *releaseIndex) checkNumbering() error {
	for i, v := range slices.Sorted(maps.Keys(r.releaseOf)) {
		if v != uint64(i)+2 {
			return fmt.Errorf("release migrations must be numbered consecutively from 002: %s", r.firstFile[v])
		}
	}
	return nil
}
