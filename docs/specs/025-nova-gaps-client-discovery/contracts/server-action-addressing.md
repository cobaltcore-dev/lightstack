# Contract: addressing an action by name (US5 / B14)

**Covers**: FR-029 – FR-033
**Applies to**: `POST /v2.1/servers/{ref}/action`, every action the dispatcher dispatches

## What is actually wrong

The dispatch table's signature already carries the resolved id and the owning project to every entry:

```go
var serverActions = map[string]func(*Service, *gin.Context, any, string, string){ ... }
//                                                            ^id     ^owning project
```

`ServerAction` resolves and authorizes once (`nova/handlers.go:1615-1621`) and passes both down. Handlers
that need them already receive them (`pause`, `lock`, `forceDelete`, `rebuild`, `rescue`). The 17 broken
ones declare `func(c *gin.Context)`, discard the values as `_, _`, and re-read `c.Param("id")` — the raw,
**unresolved** reference. Against a `uuid` column a name fails the cast, misses the `pgx.ErrNoRows`
branch and lands on the generic `500`.

SPEC-023's own comment at `handlers.go:1525` says these were "left as-is so this refactor changes dispatch
only" — it deferred exactly this.

## The change

```go
// before
func (svc *Service) SuspendInstance(c *gin.Context) { instanceID := c.Param("id"); … }
"suspend": func(s *Service, c *gin.Context, _ any, _, _ string) { s.SuspendInstance(c) },

// after
func (svc *Service) SuspendInstance(c *gin.Context, instanceID, projectID string) { … }
"suspend": func(s *Service, c *gin.Context, _ any, id, proj string) { s.SuspendInstance(c, id, proj) },
```

**Add no lookups** (FR-031). Resolving inside each handler would resolve twice per request and reintroduce
the divergence SPEC-023 removed.

## The 17 handlers

`SuspendInstance:20`, `ResumeInstance:92`, `ShelveInstance:163`, `UnshelveInstance:236`,
`ResizeInstance:283`, `ConfirmResizeInstance:407`, `RevertResizeInstance:434`, `EvacuateInstance:486`,
`MigrateInstance:521`, `LiveMigrateInstance:609`, `AddSecurityGroup:780`, `RemoveSecurityGroup:868`,
`ChangePassword:974`, `RestoreInstance:1053`, `CreateBackupAction:1096`, `ResetStateAction:1236`,
`ResetNetworkAction:1301` — all in `internal/nova/advanced_actions.go`.

**Verified**: none is reachable by any route other than the dispatcher, so no call site needs its own
resolution and authorization is untouched (FR-032).

## Answers

| Reference | Caller | Answer |
|---|---|---|
| id or name, own project | owner | the action performs (FR-029) |
| unknown name | anyone | *absent* — never `500` (FR-030) |
| name existing in another project | non-administrator | *absent* (FR-032) |
| id in another project | administrator | performs, override unchanged (FR-032) |

## Test obligations

- Contract test driving **every** action by name over the API directly. The CLI resolves names
  client-side, which is why no suite has ever caught this — a CLI-driven test cannot reproduce it.
- A name that matches a server in another project resolves to *absent*, not to that server.
