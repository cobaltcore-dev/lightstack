package neutron

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
)

// A security group as upstream's API presents it (SPEC-033 round 2: FR-019a, FR-019b, FR-024–FR-026,
// FR-050, FR-051; research R38, R39, R46, R47, R53).

// securityGroupRow is one row of security_groups as the API renders it.
type securityGroupRow struct {
	id, name, description, projectID string
	stateful                         bool
	revision                         int
	createdAt, updatedAt             time.Time
}

// securityGroupColumns is the select list scanSecurityGroup reads, in its order.
const securityGroupColumns = `id::text, name, COALESCE(description, ''), COALESCE(project_id::text, ''),
	stateful, revision_number, created_at, updated_at`

func scanSecurityGroup(s interface{ Scan(dest ...any) error }) (securityGroupRow, error) {
	var g securityGroupRow
	err := s.Scan(&g.id, &g.name, &g.description, &g.projectID, &g.stateful, &g.revision, &g.createdAt, &g.updatedAt)
	return g, err
}

// render is the group's body in a listing, a show, a create and an update, as upstream's. Every group
// is unshared and untagged; rules is never nil, since clients iterate it unconditionally.
func (g securityGroupRow) render(rules []gin.H) gin.H {
	if rules == nil {
		rules = []gin.H{}
	}
	return gin.H{
		"id":                   g.id,
		"name":                 g.name,
		"description":          g.description,
		"project_id":           g.projectID,
		"tenant_id":            g.projectID,
		"created_at":           g.createdAt.Format(time.RFC3339),
		"updated_at":           g.updatedAt.Format(time.RFC3339),
		"security_group_rules": rules,
		"stateful":             g.stateful,
		"revision_number":      g.revision,
		"shared":               false,
		"tags":                 []string{},
	}
}

// renderSecurityGroup renders one group with its rules, read from pool.
func renderSecurityGroup(ctx context.Context, pool database.DB, g securityGroupRow) (gin.H, error) {
	rules, err := securityGroupRulesByGroup(ctx, pool, []string{g.id})
	if err != nil {
		return nil, err
	}
	return g.render(rules[g.id]), nil
}

// isDefaultName reports whether name is the reserved one, in any case, as upstream's
// _validate_name_not_default compares it.
func isDefaultName(name string) bool { return strings.ToLower(name) == "default" }

// Upstream's refusals for the default group (neutron/extensions/securitygroup.py), with their types.
func errCannotRemoveDefault() *common.OpenStackError {
	return common.NewNeutronError(http.StatusConflict, "SecurityGroupCannotRemoveDefault",
		"Insufficient rights for removing default security group.")
}

func errCannotUpdateDefault() *common.OpenStackError {
	return common.NewNeutronError(http.StatusConflict, "SecurityGroupCannotUpdateDefault",
		"Updating default security group not allowed.")
}

func errDefaultAlreadyExists() *common.OpenStackError {
	return common.NewNeutronError(http.StatusConflict, "SecurityGroupDefaultAlreadyExists",
		"Default security group already exists.")
}

// errSecurityGroupInUse is upstream's SecurityGroupInUse, for a delete or a change of statefulness of
// a group a port carries, raised without a reason as upstream raises it there.
func errSecurityGroupInUse(id string) *common.OpenStackError {
	return common.NewNeutronError(http.StatusConflict, "SecurityGroupInUse", fmt.Sprintf("Security Group %s in use.", id))
}

// conflictReason is upstream's reason for refusing a port both kinds of group
// (neutron/db/securitygroups_db.py, _validate_sgs_for_port).
const conflictReason = "Cannot apply both stateful and stateless security groups on the same port at the same time."

// errSecurityGroupConflict is upstream's SecurityGroupConflict, worded as upstream's message format
// words it ("Error %(reason)s while attempting the operation.").
func errSecurityGroupConflict() *common.OpenStackError {
	return common.NewNeutronError(http.StatusConflict, "SecurityGroupConflict",
		"Error "+conflictReason+" while attempting the operation.")
}

// ErrSecurityGroupConflict reports a port that would carry stateful and stateless groups at once.
// Neutron answers it 409 SecurityGroupConflict; compute answers addSecurityGroup with 409, and fails a
// boot's port allocation with it, as upstream's (FR-051). Its message is upstream's reason, which a
// tenant may read in a failed boot's fault.
var ErrSecurityGroupConflict = &tenantVisibleError{msg: conflictReason}

// requireOneStatefulness refuses a port whose groups are of both kinds. It runs after the caller has
// written the port's groups, in the same transaction: each written row's foreign key has taken its
// group's key-share lock, which a change of statefulness (SELECT … FOR UPDATE) waits for, so a group
// cannot change kind between this read and the commit.
func requireOneStatefulness(ctx context.Context, tx database.Tx, portID string) error {
	var kinds int
	if err := tx.QueryRow(ctx, `
		SELECT count(DISTINCT sg.stateful)
		  FROM port_security_groups psg JOIN security_groups sg ON sg.id = psg.security_group_id
		 WHERE psg.port_id = $1`, portID).Scan(&kinds); err != nil {
		return fmt.Errorf("reading the kinds of port %s's groups: %w", portID, err)
	}
	if kinds > 1 {
		return ErrSecurityGroupConflict
	}
	return nil
}

// AmbiguousSecurityGroupError reports a name that several of the project's groups share. Compute
// answers it 409 with upstream Nova's NoUniqueMatch message, which is the network client's (FR-026).
type AmbiguousSecurityGroupError struct{ Name string }

func (e *AmbiguousSecurityGroupError) Error() string {
	return fmt.Sprintf("Multiple security_group matches found for name '%s', use an ID to be more specific.", e.Name)
}

// invalidInput is upstream's InvalidInput, which its attribute converters raise.
func invalidInput(message string) *common.OpenStackError {
	return common.NewNeutronError(http.StatusBadRequest, "InvalidInput", "Invalid input for operation: "+message+".")
}

// queryBooleans parses repeated boolean query values as upstream's convert_to_boolean does —
// strutils.bool_from_string, strict: the words common.ParseQueryBool knows (SPEC-033 T197) — and
// returns the distinct values given. Any other answers 400 InvalidInput, as upstream's.
func queryBooleans(values []string) ([]bool, *common.OpenStackError) {
	var out []bool
	for _, v := range values {
		b, ok := common.ParseQueryBool(v)
		if !ok {
			return nil, invalidInput(fmt.Sprintf("'%s' cannot be converted to boolean", v))
		}
		if !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out, nil
}

// queryIntegers parses repeated integer query values as upstream's convert_to_int does.
func queryIntegers(values []string) ([]int, *common.OpenStackError) {
	out := make([]int, 0, len(values))
	for _, v := range values {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, invalidInput(fmt.Sprintf("'%s' is not an integer", v))
		}
		out = append(out, n)
	}
	return out, nil
}

// nonEmpty drops empty values, which name no tag.
func nonEmpty(values []string) []string {
	return slices.DeleteFunc(slices.Clone(values), func(v string) bool { return v == "" })
}

// securityGroupListFilter scopes the listing and applies every attribute filter upstream's does, each
// repeatable, matching any of its values (FR-019a; research R13, R38).
//   - The scope is the caller's project, every project's for an administrator (FR-057). project_id
//     and tenant_id, merged, narrow it to the named projects; another project lists nothing for anyone
//     else (projectScope).
//   - id, name and description compare their columns; id as text, so a malformed one matches nothing.
//   - stateful and revision_number compare the stored values (FR-050, FR-051).
//   - shared, tags and tags-any answer from what is true of every group: unshared and untagged. So
//     shared=true and any tag match nothing, and not-tags and not-tags-any match everything.
//
// A boolean or an integer that does not parse answers 400 InvalidInput, as upstream's converters do.
func securityGroupListFilter(c *gin.Context) (*common.SQLFilter, *common.OpenStackError) {
	f := &common.SQLFilter{}
	projectScope(c, f, "project_id", ownedBy("project_id"))
	if refusal := filterByColumns(c, f, columnFilter{param: "id", column: "id"}, columnFilter{param: "name", column: "name"},
		columnFilter{param: "description", column: "description"}); refusal != nil {
		return nil, refusal
	}
	if values := c.QueryArray("stateful"); len(values) > 0 {
		kinds, refusal := queryBooleans(values)
		if refusal != nil {
			return nil, refusal
		}
		if len(kinds) == 1 { // both kinds asked for: every group matches
			f.Eq("stateful", kinds[0])
		}
	}
	if values := c.QueryArray("shared"); len(values) > 0 {
		asked, refusal := queryBooleans(values)
		if refusal != nil {
			return nil, refusal
		}
		if !slices.Contains(asked, false) {
			f.Cond("FALSE") // no group is shared
		}
	}
	if values := c.QueryArray("revision_number"); len(values) > 0 {
		revisions, refusal := queryIntegers(values)
		if refusal != nil {
			return nil, refusal
		}
		f.Cond("revision_number = ANY(" + f.Arg(revisions) + ")")
	}
	if len(nonEmpty(c.QueryArray("tags"))) > 0 || len(nonEmpty(c.QueryArray("tags-any"))) > 0 {
		f.Cond("FALSE") // no group has a tag
	}
	return f, nil
}

// columnFilter is a query parameter of a listing, the column it compares, and the attribute's converter,
// nil for none: it gives the value as stored, "" for none, or upstream's refusal.
type columnFilter struct {
	param, column string
	convert       func(string) (string, *common.OpenStackError)
}

// filterByColumns narrows f by each of filters a query gives, repeatable, matching any of its values:
// each value converted as upstream's get_filters_from_dict converts it, then the column compared as text,
// "" for none, so a malformed value matches nothing (FR-019a, T267). The group and rule listings both use
// it (SPEC-033 T194).
func filterByColumns(c *gin.Context, f *common.SQLFilter, filters ...columnFilter) *common.OpenStackError {
	for _, cf := range filters {
		values := c.QueryArray(cf.param)
		if len(values) == 0 {
			continue
		}
		if cf.convert != nil {
			converted := make([]string, len(values))
			for i, v := range values {
				var refusal *common.OpenStackError
				if converted[i], refusal = cf.convert(v); refusal != nil {
					return refusal
				}
			}
			values = converted
		}
		f.Cond("COALESCE(" + cf.column + "::text, '') = ANY(" + f.Arg(values) + ")")
	}
	return nil
}

// securityGroupRuleFilters are upstream's attribute filters of the rule listing, each repeatable, with the
// converters of the rule's attributes (SPEC-033 T194, T267). project_id and tenant_id narrow by the group's
// project (projectScope).
var securityGroupRuleFilters = []columnFilter{
	{param: "id", column: "sgr.id"}, {param: "security_group_id", column: "sgr.security_group_id"},
	{param: "direction", column: "sgr.direction"},
	{param: "ethertype", column: "sgr.ethertype", convert: filterValue(convertEtherType)},
	{param: "protocol", column: "sgr.protocol", convert: filterValue(convertProtocol)},
	{param: "port_range_min", column: "sgr.port_range_min", convert: convertPortValue},
	{param: "port_range_max", column: "sgr.port_range_max", convert: convertPortValue},
	{param: "remote_ip_prefix", column: "sgr.remote_ip_prefix", convert: func(v string) (string, *common.OpenStackError) {
		p, _, refusal := convertRemoteIPPrefix(&v)
		return derefOr(p, ""), refusal
	}},
	{param: "remote_group_id", column: "sgr.remote_group_id"},
}

// filterValue is a rule attribute's converter as a filter value's: none is "".
func filterValue(convert func(*string) (*string, *common.OpenStackError)) func(string) (string, *common.OpenStackError) {
	return func(v string) (string, *common.OpenStackError) {
		converted, refusal := convert(&v)
		return derefOr(converted, ""), refusal
	}
}

// convertPortValue is upstream's convert_validate_port_value: a number from 0 to 65535.
func convertPortValue(v string) (string, *common.OpenStackError) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 || n > 65535 {
		return "", ruleRefusal("SecurityGroupInvalidPortValue", fmt.Sprintf("Invalid value for port %s", v))
	}
	return strconv.Itoa(n), nil
}

// revisionETag prefixes the entity tag that carries a revision constraint.
const revisionETag = "revision_number="

// quotedETag is webob's _rx_etag: a quoted entity tag, W/ marking a weak one.
var quotedETag = regexp.MustCompile(`(?:^|\s)(W/)?"((?:\\"|.)*?)"`)

// ifMatchRevision reads an If-Match revision constraint as upstream's
// check_request_for_revision_constraint does (FR-050): of the header's entity tags, the one with the
// revision_number= prefix constrains, and a second, or one whose number does not parse, is a 400. ok
// is false when none constrains.
func ifMatchRevision(c *gin.Context) (revision int, ok bool, refusal *common.OpenStackError) {
	for _, etag := range strongETags(c.GetHeader("If-Match")) {
		if !strings.HasPrefix(etag, revisionETag) {
			continue
		}
		if ok {
			return 0, false, badRequest("if-match", "Multiple revision_number etags are not supported.")
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.Split(etag, revisionETag)[1]))
		if err != nil {
			return 0, false, badRequest("if-match", "Revision number etag must be in the format of revision_number=<int>")
		}
		revision, ok = n, true
	}
	return revision, ok, nil
}

// strongETags lists the entity tags upstream's request.if_match holds, as webob's
// ETagMatcher.parse(header, strong=True) reads them: the quoted ones but the weak, or the whole header
// when none is quoted. An absent header and * carry no revision either way.
func strongETags(header string) []string {
	quoted := quotedETag.FindAllStringSubmatch(header, -1)
	if len(quoted) == 0 {
		return []string{header}
	}
	var etags []string
	for _, m := range quoted {
		if m[1] == "" {
			etags = append(etags, m[2])
		}
	}
	return etags
}

// errRevisionConstraint is upstream's RevisionNumberConstraintFailed: 412, nothing changed.
func errRevisionConstraint(expected, current int) *common.OpenStackError {
	return common.NewNeutronError(http.StatusPreconditionFailed, "RevisionNumberConstraintFailed",
		fmt.Sprintf("Constrained to %d, but current revision is %d", expected, current))
}

// bumpRevision adds one to a group's revision in the caller's transaction, as upstream's revision
// plugin does for every change of the group or of its rules, and returns the new value, which an undo
// compares against.
func bumpRevision(ctx context.Context, tx database.Tx, groupID string) (int, error) {
	var revision int
	err := tx.QueryRow(ctx,
		"UPDATE security_groups SET revision_number = revision_number + 1 WHERE id::text = $1 RETURNING revision_number",
		groupID).Scan(&revision)
	if errors.Is(err, database.ErrNoRows) {
		return 0, common.NewNotFoundError("security group")
	}
	if err != nil {
		return 0, fmt.Errorf("bump the revision of security group %s: %w", groupID, err)
	}
	return revision, nil
}

// unbumpRevision puts back a revision a change bumped to bumped, compare-and-set: a revision another
// change has moved on since stays where that change left it (FR-050).
func unbumpRevision(ctx context.Context, tx database.Tx, groupID string, bumped int) error {
	if _, err := tx.Exec(ctx,
		"UPDATE security_groups SET revision_number = revision_number - 1 WHERE id::text = $1 AND revision_number = $2",
		groupID, bumped); err != nil {
		return fmt.Errorf("put back the revision of security group %s: %w", groupID, err)
	}
	return nil
}
