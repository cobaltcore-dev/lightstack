# Contract: security-group rule shape (US5 / B9)

**Covers**: FR-026, FR-027, FR-028, FR-029
**Applies to**: the four places a rule is described

| # | Emitter | Location | State |
|---|---|---|---|
| 1 | `ListSecurityGroupRules` | `ports.go:1081` | aligned (#533) |
| 2 | `GetSecurityGroupRule` | `ports.go` | aligned (#533) |
| 3 | `securityGroupRulesByGroup` | `ports.go` | aligned (#533) |
| 4 | **`CreateSecurityGroupRule`** | `ports.go:1042-1053` | **wrong** |

The test that proves it is `TestRuleValuesAgreeAcrossAllThreeEmitters`
(`security_group_shape_test.go:245`). The name is the evidence: #533 aligned three and the create path
was never in scope.

## Required shape

```json
{
  "security_group_rule": {
    "id": "…", "security_group_id": "…", "direction": "ingress", "ethertype": "IPv4",
    "protocol": null, "port_range_min": null, "port_range_max": null,
    "remote_ip_prefix": null, "remote_group_id": null, "created_at": "…"
  }
}
```

Every key always present (an absent key is a `KeyError` in clients that index the rule directly);
unset optional values are `null`, never `""` and never `0`.

## What emitter 4 does today

```go
portMin := 0                                                        // :968  sentinel
if req.SecurityGroupRule.PortRangeMin != nil { portMin = *…}
sql.NullInt32{Int32: int32(portMin), Valid: portMin > 0}             // :1024 writes NULL for a real 0
"port_range_min": portMin,                                          // :1048 reports 0 for unset
"remote_ip_prefix": remoteIP,                                       // :1050 reports "" for unset
```

The request struct **already** declares these as pointers (`ports.go:945-949`), so the wire
distinction arrives intact and is then flattened away. The fix is to stop flattening — carry the
pointers to the INSERT and the response.

## Three-valued behaviour

| Wire | Store | Report |
|---|---|---|
| omitted / `null` | `NULL` | `null` |
| `0` | `0` | `0` |
| `443` | `443` | `443` |

🔑 **The middle row fails today in *storage*, not just in the response.** `Valid: portMin > 0` writes
`NULL` for a deliberate `0` — a valid ICMP type. Spec A6 left this open ("if they are not, making them
so is part of US5"); it is confirmed: the columns are nullable and can hold it, the handler cannot
express it. FR-028 is therefore a write-path change.

Same conflation for strings: `Valid: remoteIP != ""` cannot distinguish unset from an explicit `""`.

## Contract tests

| ID | Given | When | Then |
|---|---|---|---|
| **G1** | a rule created with no `remote_ip_prefix` and no `remote_group_id` | read the create response | both `null` — **fails today**, both `""` |
| **G2** | a rule created with no port range | read the create response | both bounds `null` — **fails today**, both `0` |
| **G3** | the same rule | `GET /v2.0/security-group-rules/{id}` | byte-identical value set to the create response |
| **G4** | the same rule | fetch via list, and via its group | all four descriptions agree |
| **G5** | a rule with every optional field set | compare all four | agree — the populated case must not regress |
| **G6** | `port_range_min: 0, port_range_max: 0` with `protocol: icmp` | create, then read back | both report `0`, and the **stored** values are `0`, not `NULL` |
| **G7** | the extended agreement test | `go test ./internal/neutron/` | fails before the change, passes after |
| **G8** | any rule | inspect the key set of all four | identical key sets, no key absent anywhere |

## Note on the iptables hand-off

`networking.SecurityGroupRule` (built at `ports.go:995`) takes value types, and a kernel rule has no
concept of "unset". A local dereference with an explicit default is correct **there** — that is the
one legitimate sentinel in this path. It must not leak back into the stored values or the response.
