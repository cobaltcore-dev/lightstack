package common

import (
	"reflect"
	"strconv"
	"testing"
)

// The whole point of SQLArgs is that a placeholder cannot be obtained without appending the value it
// reads, so the number and the position cannot disagree. These tests pin that invariant rather than
// the rendering, because the rendering is obvious and the numbering is what used to be wrong.

func TestSQLArgsNumbersInAppendOrder(t *testing.T) {
	var a SQLArgs
	if got := a.Arg("first"); got != "$1" {
		t.Errorf("first Arg = %q, want $1", got)
	}
	if got := a.Arg(2); got != "$2" {
		t.Errorf("second Arg = %q, want $2", got)
	}
	if got := a.Arg(true); got != "$3" {
		t.Errorf("third Arg = %q, want $3", got)
	}
	if want := []any{"first", 2, true}; !reflect.DeepEqual(a.Args(), want) {
		t.Errorf("Args() = %v, want %v", a.Args(), want)
	}
}

// $n must index Args()[n-1]. A helper that numbered from zero, or that appended before returning the
// previous number, would still look plausible in isolation — this is the check that catches it. More
// than nine values, because two-digit placeholders are where hand-built ones used to go wrong.
func TestSQLArgsPlaceholderIndexesItsOwnValue(t *testing.T) {
	var a SQLArgs
	values := []any{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa", "lambda", "mu"}
	placeholders := make([]string, 0, len(values))
	for _, v := range values {
		placeholders = append(placeholders, a.Arg(v))
	}
	for i, ph := range placeholders {
		want := "$" + strconv.Itoa(i+1)
		if ph != want {
			t.Fatalf("placeholder %d = %q, want %q", i, ph, want)
		}
		if got := a.Args()[i]; got != values[i] {
			t.Errorf("%s reads %v, want %v", ph, got, values[i])
		}
	}
}

func TestSQLFilterWhere(t *testing.T) {
	t.Run("no conditions renders nothing", func(t *testing.T) {
		var f SQLFilter
		if got := f.Where(); got != "" {
			t.Errorf("Where() = %q, want \"\" — an unfiltered query must be unchanged, not `WHERE`", got)
		}
		if f.Args() != nil {
			t.Errorf("Args() = %v, want nil", f.Args())
		}
	})

	t.Run("conditions are ANDed in order", func(t *testing.T) {
		var f SQLFilter
		f.Eq("project_id", "p1")
		f.Eq("s.name", "web")
		if want := " WHERE project_id = $1 AND s.name = $2"; f.Where() != want {
			t.Errorf("Where() = %q, want %q", f.Where(), want)
		}
		if want := []any{"p1", "web"}; !reflect.DeepEqual(f.Args(), want) {
			t.Errorf("Args() = %v, want %v", f.Args(), want)
		}
	})

	t.Run("Cond composes with Arg for what Eq cannot express", func(t *testing.T) {
		var f SQLFilter
		f.Cond("(project_id = " + f.Arg("p1") + " OR shared = true OR external = true)")
		f.Eq("id::text", "abc")
		if want := " WHERE (project_id = $1 OR shared = true OR external = true) AND id::text = $2"; f.Where() != want {
			t.Errorf("Where() = %q, want %q", f.Where(), want)
		}
	})

	t.Run("Conds omits the keyword for a query that already has WHERE", func(t *testing.T) {
		var f SQLFilter
		f.Eq("a", 1)
		f.Eq("b", 2)
		if want := "a = $1 AND b = $2"; f.Conds() != want {
			t.Errorf("Conds() = %q, want %q", f.Conds(), want)
		}
	})
}

func TestSQLUpdate(t *testing.T) {
	t.Run("empty reports no assignments", func(t *testing.T) {
		var u SQLUpdate
		if !u.Empty() {
			t.Error("Empty() = false on a fresh SQLUpdate; an UPDATE with no SET is a syntax error")
		}
	})

	t.Run("assignments are comma joined and numbered in order", func(t *testing.T) {
		var u SQLUpdate
		u.Set("name", "n")
		u.Set("enabled", false)
		u.SetExpr("updated_at = NOW()")
		if want := "name = $1, enabled = $2, updated_at = NOW()"; u.Assignments() != want {
			t.Errorf("Assignments() = %q, want %q", u.Assignments(), want)
		}
		if want := []any{"n", false}; !reflect.DeepEqual(u.Args(), want) {
			t.Errorf("Args() = %v, want %v — SetExpr must append no argument", u.Args(), want)
		}
		if u.Empty() {
			t.Error("Empty() = true after Set")
		}
	})

	// The WHERE of an UPDATE continues the same numbering as its SET list, which is the case the
	// hand-counted form got wrong most often: the id went in as $1 while the assignments already
	// held that slot.
	t.Run("a trailing predicate continues the numbering", func(t *testing.T) {
		var u SQLUpdate
		u.Set("name", "n")
		u.Set("description", "d")
		where := " WHERE id = " + u.Arg("the-id")
		if want := " WHERE id = $3"; where != want {
			t.Errorf("trailing predicate = %q, want %q", where, want)
		}
		if want := []any{"n", "d", "the-id"}; !reflect.DeepEqual(u.Args(), want) {
			t.Errorf("Args() = %v, want %v", u.Args(), want)
		}
	})
}
