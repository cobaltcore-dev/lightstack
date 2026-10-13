package database

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Translation for SQLite (research R5, contracts/sql-dialect.md §2).
//
// Callers write SQL once, in PostgreSQL syntax. The SQLite implementation rewrites the
// few constructs SQLite cannot run, on the way in, and leaves everything else — and
// every quoted string, quoted identifier and comment — exactly as written:
//
//   - casts: ::text/::varchar/::char → CAST(x AS TEXT); ::int/::integer/::bigint/
//     ::smallint → CAST(x AS INTEGER); a bare type tag on text (json, uuid, timestamp,
//     boolean) is dropped, because ids, JSON, lists and times are text on SQLite. A cast
//     that would change a value (::date truncates) or a multi-word type is refused, not
//     dropped;
//   - list membership: = ANY($n) (with or without an array cast) → IN ($k, $k+1, …),
//     one placeholder per element of the slice bound to $n, later placeholders
//     renumbered;
//   - pagination: LIMIT $n → LIMIT COALESCE($n, -1) and OFFSET $n → OFFSET
//     COALESCE($n, 0), because PostgreSQL reads a NULL there as no limit or no offset
//     and SQLite refuses it;
//   - row locks: FOR UPDATE is removed — immediate transactions serialise writers;
//   - console-token expiry: now() + make_interval(secs => $n) → ls_add_seconds(now(), $n);
//   - regular expressions: x ~ y → x REGEXP y.
//
// A statement is parsed once and cached as a template; only the list expansion runs
// per call. Anything the rules do not cover and SQLite would misread — dollar quoting,
// ~*, !~, a cast to an unknown type, ANY in another form — is an error naming the
// construct, never a silent pass-through.

type tokKind int

const (
	tkSpace  tokKind = iota // whitespace and comments
	tkIdent                 // identifiers and keywords
	tkNumber                // numeric literals
	tkString                // '…'
	tkQuoted                // "…"
	tkParam                 // $n
	tkList                  // an expanded list membership argument (output only)
	tkOp                    // ::, ->>, ->, =>, ~*, !~, <>, !=, >=, <=, ||
	tkPunct                 // any other single character
)

type token struct {
	kind tokKind
	text string
	n    int // parameter number for tkParam and tkList
}

func (t token) isIdent(word string) bool {
	return t.kind == tkIdent && strings.EqualFold(t.text, word)
}

func (t token) is(kind tokKind, text string) bool { return t.kind == kind && t.text == text }

var multiCharOps = []string{"->>", "::", "->", "=>", "~*", "!~", "<>", "!=", ">=", "<=", "||"}

func lexSQL(sql string) ([]token, error) {
	var toks []token
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			j := i
			for j < len(sql) && strings.IndexByte(" \t\n\r", sql[j]) >= 0 {
				j++
			}
			toks, i = append(toks, token{kind: tkSpace, text: sql[i:j]}), j
		case strings.HasPrefix(sql[i:], "--"):
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				j = len(sql) - i
			}
			toks, i = append(toks, token{kind: tkSpace, text: sql[i : i+j]}), i+j
		case strings.HasPrefix(sql[i:], "/*"):
			tok, j, err := lexBlockComment(sql, i)
			if err != nil {
				return nil, err
			}
			toks, i = append(toks, tok), j
		case c == '\'' || c == '"':
			tok, j, err := lexQuoted(sql, i)
			if err != nil {
				return nil, err
			}
			toks, i = append(toks, tok), j
		case c == '$':
			tok, j, err := lexParam(sql, i)
			if err != nil {
				return nil, err
			}
			toks, i = append(toks, tok), j
		case c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z'):
			tok, j := lexWord(sql, i)
			toks, i = append(toks, tok), j
		case c >= '0' && c <= '9':
			tok, j := lexNumber(sql, i)
			toks, i = append(toks, tok), j
		default:
			if op := matchOp(sql, i); op != "" {
				toks, i = append(toks, token{kind: tkOp, text: op}), i+len(op)
				continue
			}
			toks, i = append(toks, token{kind: tkPunct, text: sql[i : i+1]}), i+1
		}
	}
	return toks, nil
}

// lexBlockComment reads a /* … */ comment as one whitespace token. i points at "/*".
func lexBlockComment(sql string, i int) (token, int, error) {
	j := strings.Index(sql[i+2:], "*/")
	if j < 0 {
		return token{}, 0, fmt.Errorf("unterminated comment")
	}
	end := i + 2 + j + 2
	return token{kind: tkSpace, text: sql[i:end]}, end, nil
}

// lexQuoted reads a '…' string or "…" identifier, honouring a doubled quote as an escape. i
// points at the opening quote.
func lexQuoted(sql string, i int) (token, int, error) {
	c := sql[i]
	j := i + 1
	for {
		k := strings.IndexByte(sql[j:], c)
		if k < 0 {
			return token{}, 0, fmt.Errorf("unterminated quoted text")
		}
		j += k + 1
		if j < len(sql) && sql[j] == c { // doubled quote: an escaped quote
			j++
			continue
		}
		break
	}
	kind := tkString
	if c == '"' {
		kind = tkQuoted
	}
	return token{kind: kind, text: sql[i:j]}, j, nil
}

// lexParam reads a $n placeholder. i points at "$". Dollar quoting and $0 are refused.
func lexParam(sql string, i int) (token, int, error) {
	j := i + 1
	for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
		j++
	}
	if j == i+1 {
		return token{}, 0, fmt.Errorf("dollar quoting is not supported on SQLite")
	}
	n, _ := strconv.Atoi(sql[i+1 : j])
	if n == 0 {
		return token{}, 0, fmt.Errorf("placeholder $0 is not valid; parameters start at $1")
	}
	return token{kind: tkParam, text: sql[i:j], n: n}, j, nil
}

// lexWord reads an identifier or keyword. i points at its first character.
func lexWord(sql string, i int) (token, int) {
	j := i + 1
	for j < len(sql) && (sql[j] == '_' || (sql[j]|0x20 >= 'a' && sql[j]|0x20 <= 'z') || (sql[j] >= '0' && sql[j] <= '9')) {
		j++
	}
	return token{kind: tkIdent, text: sql[i:j]}, j
}

// lexNumber reads a numeric literal. i points at its first digit.
func lexNumber(sql string, i int) (token, int) {
	j := i + 1
	for j < len(sql) && ((sql[j] >= '0' && sql[j] <= '9') || sql[j] == '.') {
		j++
	}
	return token{kind: tkNumber, text: sql[i:j]}, j
}

// matchOp returns the multi-character operator at sql[i], or "".
func matchOp(sql string, i int) string {
	for _, o := range multiCharOps {
		if strings.HasPrefix(sql[i:], o) {
			return o
		}
	}
	return ""
}

// next returns the index of the first non-space token at or after i, or len(toks).
func next(toks []token, i int) int {
	for i < len(toks) && toks[i].kind == tkSpace {
		i++
	}
	return i
}

// trimTail drops trailing whitespace tokens from out.
func trimTail(out []token) []token {
	for len(out) > 0 && out[len(out)-1].kind == tkSpace {
		out = out[:len(out)-1]
	}
	return out
}

// needsSpace reports whether a word replacement (IN, REGEXP) appended after out needs a
// leading space to keep a token boundary — true unless out ends in whitespace or "(".
func needsSpace(out []token) bool {
	if len(out) == 0 {
		return false
	}
	last := out[len(out)-1]
	return last.kind != tkSpace && !last.is(tkPunct, "(")
}

// isValueOperator reports whether t combines with the value to its left — an arithmetic
// operator or a two-character operator. After LIMIT/OFFSET $n it changes what the NULL-to-no-
// limit rewrite means, so such a placeholder must be refused rather than silently wrapped.
func isValueOperator(t token) bool {
	if t.kind == tkPunct {
		switch t.text {
		case "+", "-", "*", "/", "%":
			return true
		}
	}
	return t.kind == tkOp
}

// castOperand splits out into what precedes a cast operand and the operand itself: the
// last placeholder, literal, (qualified) identifier, or parenthesised group with an
// optional function name.
func castOperand(out []token) (rest, operand []token, err error) {
	end := len(out)
	if end == 0 {
		return nil, nil, fmt.Errorf("cast without an operand")
	}
	last := out[end-1]
	start := end - 1
	switch last.kind {
	case tkParam, tkString, tkNumber:
	case tkIdent, tkQuoted:
		// END closes a CASE the tokenizer cannot bound; grabbing it alone would cast the word
		// END. Refuse it — the caller parenthesises the CASE, whose ) is a valid operand.
		if last.kind == tkIdent && strings.EqualFold(last.text, "END") {
			return nil, nil, fmt.Errorf("cast after END is not supported on SQLite; wrap the expression in parentheses")
		}
		for start >= 2 && out[start-1].is(tkPunct, ".") && (out[start-2].kind == tkIdent || out[start-2].kind == tkQuoted) {
			start -= 2
		}
	case tkPunct:
		if last.text != ")" {
			return nil, nil, fmt.Errorf("cast after %q is not supported on SQLite", last.text)
		}
		depth := 0
		for start = end - 1; start >= 0; start-- {
			if out[start].is(tkPunct, ")") {
				depth++
			} else if out[start].is(tkPunct, "(") {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		if start < 0 {
			return nil, nil, fmt.Errorf("unbalanced parentheses before a cast")
		}
		if start > 0 && out[start-1].kind == tkIdent {
			start--
		}
	default:
		return nil, nil, fmt.Errorf("cast after %q is not supported on SQLite", last.text)
	}
	return out[:start], out[start:end], nil
}

// castTarget reads the type after "::" starting at toks[i] and reports the SQLite cast
// to apply ("TEXT", "INTEGER", or "" to drop the cast) and the index after the type.
func castTarget(toks []token, i int) (string, int, error) {
	i = next(toks, i)
	if i >= len(toks) || toks[i].kind != tkIdent {
		return "", i, fmt.Errorf("cast without a type")
	}
	name := strings.ToLower(toks[i].text)
	i++
	// A multi-word type (character varying, double precision, timestamp with time zone, …)
	// lexes as a second identifier right after the first; the single-word reader would strand
	// it after the cast. Refuse it rather than emit broken SQL.
	if j := next(toks, i); j < len(toks) && toks[j].kind == tkIdent {
		switch strings.ToLower(toks[j].text) {
		case "varying", "precision", "with", "without":
			return "", i, fmt.Errorf("cast to the multi-word type %q is not supported on SQLite", name+" "+toks[j].text)
		}
	}
	if j := next(toks, i); j < len(toks) && toks[j].is(tkPunct, "(") { // varchar(36)
		k := j + 1
		for k < len(toks) && !toks[k].is(tkPunct, ")") {
			k++
		}
		i = k + 1
	}
	array := false
	if j := next(toks, i); j+1 < len(toks) && toks[j].is(tkPunct, "[") && toks[j+1].is(tkPunct, "]") {
		array = true
		i = j + 2
	}
	if array {
		return "", i, nil
	}
	if target, ok := castTypes[name]; ok {
		return target, i, nil
	}
	return "", i, fmt.Errorf("cast to %s is not supported on SQLite", name)
}

// castTypes maps a PostgreSQL scalar type to the SQLite cast to apply: "TEXT", "INTEGER", or ""
// to drop a type tag on text that stays text. date is deliberately absent — ::date truncates a
// timestamp, which dropping would silently turn into a full-value compare, so it is refused.
var castTypes = map[string]string{
	"text": "TEXT", "varchar": "TEXT", "char": "TEXT", "character": "TEXT",
	"int": "INTEGER", "integer": "INTEGER", "int2": "INTEGER", "int4": "INTEGER",
	"int8": "INTEGER", "bigint": "INTEGER", "smallint": "INTEGER",
	"jsonb": "", "json": "", "uuid": "", "timestamp": "", "timestamptz": "", "boolean": "", "bool": "",
}

func translateTokens(toks []token) ([]token, error) {
	var out []token
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		var err error
		switch {
		case t.is(tkOp, "::"):
			out, i, err = translateCast(out, toks, i)

		case t.is(tkPunct, "=") && next(toks, i+1) < len(toks) && toks[next(toks, i+1)].isIdent("ANY"):
			out, i, err = translateAnyList(out, toks, i)

		case (t.isIdent("LIMIT") || t.isIdent("OFFSET")) && next(toks, i+1) < len(toks) && toks[next(toks, i+1)].kind == tkParam:
			out, i, err = translateLimitParam(out, toks, i)

		case t.isIdent("ANY"):
			return nil, fmt.Errorf("ANY is supported on SQLite only as = ANY($n)")

		case t.isIdent("FOR") && next(toks, i+1) < len(toks) && toks[next(toks, i+1)].isIdent("UPDATE"):
			out, i, err = dropForUpdate(out, toks, i)

		case t.isIdent("FOR") && next(toks, i+1) < len(toks) && toks[next(toks, i+1)].isIdent("SHARE"):
			return nil, fmt.Errorf("FOR SHARE is not supported on SQLite")

		case t.isIdent("make_interval"):
			out, i, err = translateMakeInterval(out, toks, i)

		case t.is(tkPunct, "~"):
			out = translateRegexp(out, toks, i)

		case t.is(tkOp, "~*"), t.is(tkOp, "!~"):
			return nil, fmt.Errorf("the %s operator is not supported on SQLite", t.text)

		default:
			out = append(out, t)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// translateCast rewrites the cast at toks[i] ("::") over the operand already in out: ::text →
// CAST(x AS TEXT), a bare type tag dropped. The returned index is the cast's last token.
func translateCast(out, toks []token, i int) ([]token, int, error) {
	rest, operand, err := castOperand(trimTail(out))
	if err != nil {
		return nil, 0, err
	}
	target, after, err := castTarget(toks, i+1)
	if err != nil {
		return nil, 0, err
	}
	// rest and operand share out's array; copy the operand before appending.
	operand = append([]token(nil), operand...)
	out = rest[:len(rest):len(rest)]
	if target == "" {
		out = append(out, operand...)
	} else {
		out = append(out, token{kind: tkIdent, text: "CAST"}, token{kind: tkPunct, text: "("})
		out = append(out, operand...)
		out = append(out, token{kind: tkSpace, text: " "}, token{kind: tkIdent, text: "AS"},
			token{kind: tkSpace, text: " "}, token{kind: tkIdent, text: target}, token{kind: tkPunct, text: ")"})
	}
	return out, after - 1, nil
}

// translateAnyList rewrites "= ANY($n)" (with an optional array cast) into "IN ($n)", expanded one
// placeholder per element at bind time. i points at "=".
func translateAnyList(out, toks []token, i int) ([]token, int, error) {
	j := next(toks, next(toks, i+1)+1)
	if j >= len(toks) || !toks[j].is(tkPunct, "(") {
		return nil, 0, fmt.Errorf("= ANY without a parenthesised argument is not supported on SQLite")
	}
	j = next(toks, j+1)
	if j >= len(toks) || toks[j].kind != tkParam {
		return nil, 0, fmt.Errorf("= ANY(…) supports only a list argument ($n) on SQLite")
	}
	param := toks[j].n
	j = next(toks, j+1)
	if j < len(toks) && toks[j].is(tkOp, "::") {
		_, after, err := castTarget(toks, j+1)
		if err != nil {
			return nil, 0, err
		}
		j = next(toks, after)
	}
	if j >= len(toks) || !toks[j].is(tkPunct, ")") {
		return nil, 0, fmt.Errorf("= ANY(…) supports only a list argument ($n) on SQLite")
	}
	if needsSpace(out) { // "id=ANY($1)" must not become "idIN (…)"
		out = append(out, token{kind: tkSpace, text: " "})
	}
	out = append(out, token{kind: tkIdent, text: "IN"}, token{kind: tkSpace, text: " "},
		token{kind: tkPunct, text: "("}, token{kind: tkList, n: param}, token{kind: tkPunct, text: ")"})
	return out, j, nil
}

// translateLimitParam wraps a "LIMIT $n"/"OFFSET $n" placeholder in COALESCE so a NULL reads as no
// limit/offset, as PostgreSQL does. i points at LIMIT/OFFSET.
func translateLimitParam(out, toks []token, i int) ([]token, int, error) {
	j := next(toks, i+1)
	if k := next(toks, j+1); k < len(toks) && isValueOperator(toks[k]) {
		// "LIMIT $1 + 1" with $1 NULL would wrap to COALESCE($1,-1)+1 = 0, where PostgreSQL
		// reads the whole expression as NULL (no limit). Refuse it.
		return nil, 0, fmt.Errorf("%s $n followed by %q is not supported on SQLite", strings.ToUpper(toks[i].text), toks[k].text)
	}
	none := "-1" // SQLite's "no limit"
	if toks[i].isIdent("OFFSET") {
		none = "0"
	}
	out = append(out, toks[i:j]...)
	out = append(out, token{kind: tkIdent, text: "COALESCE"}, token{kind: tkPunct, text: "("}, toks[j],
		token{kind: tkPunct, text: ","}, token{kind: tkSpace, text: " "}, token{kind: tkNumber, text: none},
		token{kind: tkPunct, text: ")"})
	return out, j, nil
}

// dropForUpdate removes a trailing "FOR UPDATE" — immediate transactions serialise writers — and
// refuses its OF/SKIP/NOWAIT variants. i points at FOR.
func dropForUpdate(out, toks []token, i int) ([]token, int, error) {
	j := next(toks, i+1)
	if k := next(toks, j+1); k < len(toks) && (toks[k].isIdent("OF") || toks[k].isIdent("SKIP") || toks[k].isIdent("NOWAIT")) {
		return nil, 0, fmt.Errorf("FOR UPDATE %s is not supported on SQLite", strings.ToUpper(toks[k].text))
	}
	return trimTail(out), j, nil
}

// translateMakeInterval rewrites "now() + make_interval(secs => $n)" — the one form in use — into
// "ls_add_seconds(now(), $n)". i points at make_interval.
func translateMakeInterval(out, toks []token, i int) ([]token, int, error) {
	tail := trimTail(out)
	n := len(tail)
	ok := n >= 4 && tail[n-1].is(tkPunct, "+")
	if ok {
		pre := trimTail(tail[:n-1])
		m := len(pre)
		ok = m >= 3 && pre[m-1].is(tkPunct, ")") && pre[m-2].is(tkPunct, "(") && pre[m-3].isIdent("now")
		if ok {
			tail = pre[:m-3]
		}
	}
	j := next(toks, i+1)
	var param int
	if ok && j < len(toks) && toks[j].is(tkPunct, "(") {
		j = next(toks, j+1)
		ok = j < len(toks) && toks[j].isIdent("secs")
		j = next(toks, j+1)
		ok = ok && j < len(toks) && toks[j].is(tkOp, "=>")
		j = next(toks, j+1)
		ok = ok && j < len(toks) && toks[j].kind == tkParam
		if ok {
			param = toks[j].n
		}
		j = next(toks, j+1)
		ok = ok && j < len(toks) && toks[j].is(tkPunct, ")")
	} else {
		ok = false
	}
	if !ok {
		return nil, 0, fmt.Errorf("make_interval is supported on SQLite only as now() + make_interval(secs => $n)")
	}
	repl := []token{{kind: tkIdent, text: "ls_add_seconds"},
		{kind: tkPunct, text: "("}, {kind: tkIdent, text: "now"}, {kind: tkPunct, text: "("},
		{kind: tkPunct, text: ")"}, {kind: tkPunct, text: ","}, {kind: tkSpace, text: " "},
		{kind: tkParam, text: "$" + strconv.Itoa(param), n: param}, {kind: tkPunct, text: ")"}}
	if len(tail) > 0 && tail[len(tail)-1].kind != tkSpace && !tail[len(tail)-1].is(tkPunct, "(") {
		repl = append([]token{{kind: tkSpace, text: " "}}, repl...)
	}
	return append(tail[:len(tail):len(tail)], repl...), j, nil
}

// translateRegexp replaces "~" with REGEXP, keeping a word boundary on each side so "x~y" does not
// become "xREGEXPy". i points at "~".
func translateRegexp(out, toks []token, i int) []token {
	if needsSpace(out) {
		out = append(out, token{kind: tkSpace, text: " "})
	}
	out = append(out, token{kind: tkIdent, text: "REGEXP"})
	if i+1 < len(toks) && toks[i+1].kind != tkSpace {
		out = append(out, token{kind: tkSpace, text: " "})
	}
	return out
}

// sqlTemplate is a translated statement: literal text interleaved with parameter
// references, some of which expand to lists.
type sqlTemplate struct {
	segs    []segment
	hasList bool
}

type segment struct {
	text  string
	param int // 0 for literal text
	list  bool
}

func compileTemplate(sql string) (*sqlTemplate, error) {
	toks, err := lexSQL(sql)
	if err != nil {
		return nil, err
	}
	toks, err = translateTokens(toks)
	if err != nil {
		return nil, err
	}
	tp := &sqlTemplate{}
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			tp.segs = append(tp.segs, segment{text: b.String()})
			b.Reset()
		}
	}
	for _, t := range toks {
		switch t.kind {
		case tkParam:
			flush()
			tp.segs = append(tp.segs, segment{param: t.n})
		case tkList:
			flush()
			tp.segs = append(tp.segs, segment{param: t.n, list: true})
			tp.hasList = true
		default:
			b.WriteString(t.text)
		}
	}
	flush()
	return tp, nil
}

// expand renders the template for one call's arguments. Without a list, placeholders
// and arguments pass through as written; with one, every placeholder is renumbered in
// order of appearance and the arguments are rebuilt to match.
func (tp *sqlTemplate) expand(args []any) (string, []any, error) {
	used := make([]bool, len(args)+1)
	var b strings.Builder
	var out []any
	if !tp.hasList {
		// Clone, never alias: prepareSQLite converts the returned values in place, and a
		// caller that reuses its argument slice must not have it rewritten underneath.
		out = slices.Clone(args)
	}
	seen := map[int]int{}
	for _, s := range tp.segs {
		if s.param == 0 {
			b.WriteString(s.text)
			continue
		}
		if s.param > len(args) {
			return "", nil, fmt.Errorf("statement uses $%d but %d argument(s) were given", s.param, len(args))
		}
		used[s.param] = true
		switch {
		case !tp.hasList:
			b.WriteString("$" + strconv.Itoa(s.param))
		case s.list:
			var err error
			if out, err = appendList(&b, out, args[s.param-1], s.param); err != nil {
				return "", nil, err
			}
		default:
			idx, ok := seen[s.param]
			if !ok {
				out = append(out, args[s.param-1])
				idx = len(out)
				seen[s.param] = idx
			}
			b.WriteString("$" + strconv.Itoa(idx))
		}
	}
	if err := checkAllUsed(used); err != nil {
		return "", nil, err
	}
	if out == nil {
		out = []any{}
	}
	return b.String(), out, nil
}

// appendList renders a $n list argument — one placeholder per element, renumbered to follow the
// arguments already in out — and returns out with those elements appended.
func appendList(b *strings.Builder, out []any, arg any, param int) ([]any, error) {
	v := reflect.ValueOf(arg)
	if v.Kind() != reflect.Slice || v.Type().Elem().Kind() == reflect.Uint8 {
		return nil, fmt.Errorf("= ANY($%d) needs a list argument, got %T", param, arg)
	}
	for k := 0; k < v.Len(); k++ {
		if k > 0 {
			b.WriteString(", ")
		}
		out = append(out, v.Index(k).Interface())
		b.WriteString("$" + strconv.Itoa(len(out)))
	}
	return out, nil
}

// checkAllUsed reports the first 1-based argument the statement never referenced, so an unused
// argument is an error rather than a silent mismatch.
func checkAllUsed(used []bool) error {
	for i := 1; i < len(used); i++ {
		if !used[i] {
			return fmt.Errorf("argument $%d is not used by the statement", i)
		}
	}
	return nil
}

const translateCacheCap = 4096

var (
	translateMu    sync.Mutex
	translateCache = map[string]*sqlTemplate{}
)

// rewrite translates one statement and its arguments for SQLite.
func rewrite(sql string, args []any) (string, []any, error) {
	translateMu.Lock()
	tp, ok := translateCache[sql]
	translateMu.Unlock()
	if !ok {
		var err error
		if tp, err = compileTemplate(sql); err != nil {
			return "", nil, fmt.Errorf("translate for SQLite: %w", err)
		}
		translateMu.Lock()
		if len(translateCache) >= translateCacheCap {
			translateCache = map[string]*sqlTemplate{}
		}
		translateCache[sql] = tp
		translateMu.Unlock()
	}
	return tp.expand(args)
}

func resetTranslateCache() {
	translateMu.Lock()
	translateCache = map[string]*sqlTemplate{}
	translateMu.Unlock()
}

func translateCacheLen() int {
	translateMu.Lock()
	defer translateMu.Unlock()
	return len(translateCache)
}
