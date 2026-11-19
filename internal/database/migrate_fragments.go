package database

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Between releases, a schema change is a fragment in migrations/next/, named by its creation time,
// its ticket and its purpose instead of a number (SPEC-032 FR-014–FR-017, data-model §7). Changes
// made in parallel therefore never compete for a number and merge in either order. The view
// assembles the fragments into one open release migration per backend, numbered one above the
// newest numbered migration; the release freezes them into that number.

// fragmentDir is the folder of fragments inside the migrations folder.
const fragmentDir = "next"

const fragmentGrammar = "a fragment name must be <UTC YYYYMMDDhhmmss>-<ticket>-<purpose>[.postgres|.sqlite].(up|down).sql"

var fragmentFileName = regexp.MustCompile(`^([0-9]{14})-([0-9]+)-([a-z0-9]+(?:-[a-z0-9]+)*)(?:\.(postgres|sqlite))?\.(up|down)\.sql$`)

// fragment is one file of migrations/next/.
type fragment struct {
	file      string  // the file name
	name      string  // <time>-<ticket>-<purpose>: fragments apply in the byte order of their names
	backend   Backend // "" for a plain fragment, which both backends run
	direction string  // "up" or "down"
}

func parseFragment(file string) (fragment, bool) {
	m := fragmentFileName.FindStringSubmatch(file)
	if m == nil {
		return fragment{}, false
	}
	return fragment{file: file, name: m[1] + "-" + m[2] + "-" + m[3], backend: Backend(m[4]), direction: m[5]}, true
}

// otherDirection is the direction of f's counterpart.
func (f fragment) otherDirection() string {
	if f.direction == "up" {
		return "down"
	}
	return "up"
}

// counterpart is the file name of f's other direction.
func (f fragment) counterpart() string {
	return strings.TrimSuffix(f.file, "."+f.direction+".sql") + "." + f.otherDirection() + ".sql"
}

// readFragments reads and validates the fragments in src's next/ directory and returns them in
// the order they apply: by name, then by file name. Dot-files such as .gitkeep are not fragments;
// any other entry must be one (contracts/migrations-folder.md). A folder without next/ has none.
func readFragments(src fs.FS) ([]fragment, error) {
	entries, err := fs.ReadDir(src, fragmentDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read migrations/%s: %w", fragmentDir, err)
	}
	var frags []fragment
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		f, ok := parseFragment(e.Name())
		if !ok || e.IsDir() {
			return nil, fmt.Errorf("%s/%s: %s", fragmentDir, e.Name(), fragmentGrammar)
		}
		frags = append(frags, f)
	}
	if err := checkFragmentPairs(frags); err != nil {
		return nil, err
	}
	slices.SortFunc(frags, func(a, b fragment) int {
		return cmp.Or(strings.Compare(a.name, b.name), strings.Compare(a.file, b.file))
	})
	return frags, nil
}

// checkFragmentPairs refuses a fragment without its other direction, and a name that is both plain
// and backend-specific.
func checkFragmentPairs(frags []fragment) error {
	files := map[string]bool{}
	variants := map[string]map[Backend]bool{}
	for _, f := range frags {
		files[f.file] = true
		if variants[f.name] == nil {
			variants[f.name] = map[Backend]bool{}
		}
		variants[f.name][f.backend] = true
	}
	for _, f := range frags {
		if !files[f.counterpart()] {
			return fmt.Errorf("fragment %s/%s has no %s part", fragmentDir, f.file, f.otherDirection())
		}
	}
	for _, name := range slices.Sorted(maps.Keys(variants)) {
		for _, b := range []Backend{Postgres, SQLite} {
			if variants[name][""] && variants[name][b] {
				return fmt.Errorf("fragment %s exists both plain and as .%s", name, b)
			}
		}
	}
	return nil
}

// openMigration is one backend's open release migration: the fragments assembled under the next
// version, as the view serves them (research R7).
type openMigration struct {
	version          uint64
	up, down         []byte
	upBody, downBody []byte // what the release step freezes, under its own header
}

const openHeader = "open release migration %03d (unreleased): assembled from migrations/next/"

// assembleOpen assembles backend's open release migration from src's fragments, or returns nil
// when next/ holds none. Its BODY is the plain fragments and backend's own, in order, each under a
// comment naming its file; the down file undoes them newest first. The version is one above the
// newest numbered migration of either backend, and both backends get it as soon as any fragment
// exists — a backend with no fragment of its own gets the plain ones, or an empty body — so a
// recorded version names the same change on both.
func assembleOpen(src fs.FS, backend Backend) (*openMigration, error) {
	frags, err := readFragments(src)
	if err != nil || len(frags) == 0 {
		return nil, err
	}
	newest, err := newestNumbered(src)
	if err != nil {
		return nil, err
	}
	up, down := splitFragments(frags, backend)
	upBody, err := fragmentBody(src, up)
	if err != nil {
		return nil, err
	}
	downBody, err := fragmentBody(src, down)
	if err != nil {
		return nil, err
	}
	version := newest + 1
	header := fmt.Sprintf(openHeader, version)
	return &openMigration{
		version:  version,
		up:       renderUp(header, version, upBody),
		down:     renderDown(header, version, downBody),
		upBody:   upBody,
		downBody: downBody,
	}, nil
}

// splitFragments returns the fragments backend runs, plain and its own: the up parts in the order
// they apply, the down parts in reverse, newest first.
func splitFragments(frags []fragment, backend Backend) (up, down []fragment) {
	for _, f := range frags {
		switch {
		case f.backend != "" && f.backend != backend:
		case f.direction == "up":
			up = append(up, f)
		default:
			down = append(down, f)
		}
	}
	slices.Reverse(down)
	return up, down
}

// fragmentBody concatenates the fragments' SQL, each under a comment naming its file and ending
// with a newline.
func fragmentBody(src fs.FS, frags []fragment) ([]byte, error) {
	var b bytes.Buffer
	for _, f := range frags {
		sql, err := fs.ReadFile(src, path.Join(fragmentDir, f.file))
		if err != nil {
			return nil, fmt.Errorf("read migrations/%s/%s: %w", fragmentDir, f.file, err)
		}
		fmt.Fprintf(&b, "-- fragment: %s\n", f.file)
		writeBody(&b, sql)
	}
	return b.Bytes(), nil
}

// newestNumbered is the newest version among src's numbered migration files, of either backend.
func newestNumbered(src fs.FS) (uint64, error) {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return 0, fmt.Errorf("read migrations: %w", err)
	}
	var newest uint64
	for _, e := range entries {
		if f, ok := parseMigrationFile(e.Name()); ok && !e.IsDir() && f.version > newest {
			newest = f.version
		}
	}
	return newest, nil
}

// releaseName is the name of a release: v<semver>, optionally with a pre-release
// (contracts/migrations-folder.md).
var releaseName = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)

// FreezeResult is what Freeze did.
type FreezeResult struct {
	Release string
	Version uint64   // the release migration's version; 0 when the release adds none
	Written []string // the files written, sorted
	Removed []string // the fragment files removed, sorted
}

// String reports the result as the release step prints it.
func (r FreezeResult) String() string {
	if r.Version == 0 {
		return fmt.Sprintf("no fragments in migrations/next/: release %s adds no migration", r.Release)
	}
	return fmt.Sprintf("froze %d fragment files into release migration %03d (%s): %s", len(r.Removed), r.Version,
		r.Release, strings.Join(r.Written, ", "))
}

// Freeze freezes the open release migration of migrationsPath into the release migration
// NNN_<release> and empties migrations/next/ of fragments (FR-023, FR-024). The release files carry
// the open migration's BODY and checksum under a release header, so a database that applied the
// open state carries on (FR-021): one plain pair when every fragment is plain, a pair per backend
// otherwise. With no fragments the release adds no migration and nothing is written. It needs
// neither a configuration nor a database.
func Freeze(migrationsPath, release string) (FreezeResult, error) {
	res := FreezeResult{Release: release}
	if !releaseName.MatchString(release) {
		return res, fmt.Errorf("release name must be v<semver>, e.g. v0.7.0 or v0.7.0-rc.1: %q", release)
	}
	src := os.DirFS(migrationsPath)
	if err := validateFolderNames(src); err != nil {
		return res, err
	}
	if file, ok := releaseMigration(src, release); ok {
		return res, fmt.Errorf("release %s already has migration %s", release, file)
	}
	frags, err := readFragments(src)
	if err != nil || len(frags) == 0 {
		return res, err
	}
	opens := map[Backend]*openMigration{}
	for _, b := range []Backend{Postgres, SQLite} {
		if opens[b], err = assembleOpen(src, b); err != nil {
			return res, err
		}
	}
	version := opens[Postgres].version
	files := renderRelease(opens, frags, version, release)
	if res.Written, res.Removed, err = commitFreeze(migrationsPath, files, frags); err != nil {
		return res, err
	}
	res.Version = version
	return res, nil
}

// renderRelease renders the release migration's files from the open migrations: one plain pair when
// every fragment is plain, a pair per backend otherwise.
func renderRelease(opens map[Backend]*openMigration, frags []fragment, version uint64, release string) map[string][]byte {
	header := fmt.Sprintf("release migration %03d (%s): frozen from migrations/next/ by lightstack-migrate freeze", version, release)
	variants := map[Backend]string{Postgres: "." + string(Postgres), SQLite: "." + string(SQLite)}
	if !slices.ContainsFunc(frags, func(f fragment) bool { return f.backend != "" }) {
		variants = map[Backend]string{Postgres: ""} // both backends' bodies are the plain fragments
	}
	files := map[string][]byte{}
	for b, suffix := range variants {
		files[fmt.Sprintf("%03d_%s%s.up.sql", version, release, suffix)] = renderUp(header, version, opens[b].upBody)
		files[fmt.Sprintf("%03d_%s%s.down.sql", version, release, suffix)] = renderDown(header, version, opens[b].downBody)
	}
	return files
}

// commitFreeze writes the release files and removes the frozen fragments, all or nothing: on a
// failure it removes the files it wrote and writes back the fragments it removed, so the folder is
// as it was. It returns the files written and the fragments removed, each sorted.
func commitFreeze(migrationsPath string, files map[string][]byte, frags []fragment) (written, removed []string, err error) {
	saved := map[string][]byte{}
	for _, f := range frags {
		if saved[f.file], err = os.ReadFile(filepath.Join(migrationsPath, fragmentDir, f.file)); err != nil {
			return nil, nil, fmt.Errorf("read the fragment to freeze: %w", err)
		}
	}
	defer func() {
		if err != nil {
			err = undoFreeze(migrationsPath, written, removed, saved, err)
			written, removed = nil, nil
		}
	}()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err = writeNewFile(filepath.Join(migrationsPath, name), files[name]); err != nil {
			return written, removed, fmt.Errorf("write the release migration: %w", err)
		}
		written = append(written, name)
	}
	for _, f := range frags {
		if err = os.Remove(filepath.Join(migrationsPath, fragmentDir, f.file)); err != nil {
			return written, removed, fmt.Errorf("remove the frozen fragment: %w", err)
		}
		removed = append(removed, f.file)
	}
	slices.Sort(removed)
	return written, removed, nil
}

// undoFreeze puts the folder back after a failed freeze: it removes the release files written and
// writes back the fragments removed. The error says whether that worked, and if not, what to restore.
func undoFreeze(migrationsPath string, written, removed []string, saved map[string][]byte, cause error) error {
	var failed []error
	for _, name := range written {
		if err := os.Remove(filepath.Join(migrationsPath, name)); err != nil {
			failed = append(failed, err)
		}
	}
	for _, file := range removed {
		if err := writeNewFile(filepath.Join(migrationsPath, fragmentDir, file), saved[file]); err != nil {
			failed = append(failed, err)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w; rolling the freeze back failed too (%w): delete %s and restore the fragments with "+
			"git checkout -- %s", cause, errors.Join(failed...), strings.Join(written, ", "),
			filepath.Join(migrationsPath, fragmentDir))
	}
	return fmt.Errorf("%w; the freeze was rolled back and changed nothing", cause)
}

// releaseMigration is a numbered file of src that names release, its up file when there is one.
func releaseMigration(src fs.FS, release string) (string, bool) {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return "", false
	}
	found := ""
	for _, e := range entries {
		f, ok := parseMigrationFile(e.Name())
		switch {
		case !ok || f.ident != release:
		case f.direction == "up":
			return e.Name(), true
		case found == "":
			found = e.Name()
		}
	}
	return found, found != ""
}

// writeNewFile writes data to a file that must not exist yet, readable by all (0644). A file it
// could not write completely is removed again.
func writeNewFile(name string, data []byte) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}
