# Compute microversion audit (SPEC-025-followup US6, FR-042 / FR-043)

**Scope, per FR-042**: bounded to behaviours **the product emits**. A behaviour upstream gates but
Lightstack does not implement acquires no gate, and the advertised range (2.1 – 2.90) is not narrowed to
match what this audit found.

This document exists because FR-043 requires the outcome be written down — including what was found
gated upstream and deliberately left ungated. The next reader should not have to re-derive it, and
the audit's own first finding is what happens when nobody writes it down.

---

## Findings

### A1 — A documented gate that did not exist ✅ FIXED

`CreateBackupAction` in `advanced_actions.go` carried the comment *"Microversion 2.45+ returns
image_id in response body"* directly above a `c.JSON` that returned the body **unconditionally**.
Upstream introduced the `image_id` body at **2.45**; before it the image is identified by the
`Location` header alone.

So a client negotiating 2.1 received a field upstream added 44 versions later, and the code said so
while doing the opposite. Now gated by `wantsCreateImageIDInBody`, tested at both sides of the
boundary.

**Correction.** An earlier version of this entry attributed the comment to the **createImage**
action. It is in `CreateBackupAction`, which serves **createBackup**; `createImage` dispatches to
`CreateImageAction` in `handlers.go`, a different function. The mislabelling mattered — see A7.

### A7 — `createImage` served the pre-2.45 shape at every version ✅ FIXED, found on the host

Upstream changed **both** the `createImage` and `createBackup` actions at 2.45: from that version each
returns `{"image_id": ...}` in the body. `CreateImageAction` returned `Location` and a bare `202` at
every version — upstream's pre-2.45 shape, served unconditionally.

**The mirror image of A1**, in a handler upstream changed at the same time: A1 returned the new shape
always, A7 returned the old shape always. Neither read the negotiated version.

**Found by the real-mode host pass**, R8b/R8c, and only because those steps exercise `createImage` —
the action whose *name* matches the comment — while the gate had been applied to `createBackup`, the
action the comment is actually *in*. The audit found the comment and fixed the handler containing it;
the host found the handler the comment describes.

Strictly, A7 was out of scope by FR-042: the product did not *emit* the field, so there was no
version-gated behaviour to gate. It is fixed anyway because the helper already existed, both actions
now read one gate, and leaving one of a pair correct is a worse outcome than either fixing or
deferring both. `TestCreateImageActionsBothGated` pins the pair together so a future change cannot
gate one and forget the other.

### A2 — The version header was never reported on an API response ✅ FIXED

`OpenStack-API-Version` was set on the two version *documents* only, and always as the maximum. No
API response carried it at all, so a client had no way to learn what version had been applied.
`MicroversionMiddleware` now reports the applied version on every `/v2.1` response.

### A3 — An out-of-range version was accepted ✅ FIXED

`?OpenStack-API-Version: compute 2.99` was answered normally with maximum behaviour. Upstream
answers **406 Not Acceptable** with a message naming the supported range, which is what lets a client
negotiate down. Now refused before any handler runs, using the product's first 406 constructor.

### A4 — No header resolves to the MAXIMUM ⚠️ DELIBERATE DIVERGENCE, unfixed

Upstream's api-guide: *"If no version headers are provided, the minimum supported microversion is
assumed."* Lightstack assumes its **maximum**.

**Left as it is, deliberately** (FR-040, FR-040a; register entry 2). Adopting upstream's default
would remove behaviour from every existing Lightstack caller in a single release — every client that sends
no header would drop from 2.90 shapes to 2.1 shapes at once.

**This bounds what every gate in this audit can achieve**, and is the single most important line in
this document: with no-header resolving to the maximum, **a gate only ever affects a client that
negotiates explicitly**. A stock `openstack` invocation that pins nothing keeps receiving the newest
shape of every response. Gates are therefore correct-when-asked rather than correct-by-default.

### A5 — An unparseable header is tolerated ⚠️ DELIBERATE DIVERGENCE, unfixed

Upstream answers 400. Lightstack treats it as no negotiation and the request succeeds. This was SPEC-025's
decision, recorded in the code as A-006 and now as register entry 1: the gate exists to avoid
granting *more* than a client negotiated, and that property holds without inventing a rejection on a
surface that never had one. Pinned by `TestMicroversionMalformedTolerated` so a later change cannot
make it silently.

### A6 — Microversions were compared as FLOATS, which inverts version ordering ✅ FIXED

`requestedMicroversion` returned a `float64`. Its own comment argued this was safe because *"2.x
minor numbers do not carry into a second digit range where float comparison would misorder them"*.

**That is not true of the range this product advertises**, and the consequence is worse than the
`2.9`/`2.90` conflation this entry was first written about. A microversion's minor part counts
revisions; it is not a fraction. So **2.9 precedes 2.21 precedes 2.45** as versions, while as floats
the order is exactly reversed:

- `novaCurrentVersion` is `"2.90"`, whose float value is `2.9` — and 2.9 is itself a real upstream
  microversion, the one that introduced `locked`. As floats the two were identical;
- **both gates in this file were reachable by clients below them.** `2.9 > 2.45 > 2.21` numerically,
  so a client negotiating **2.3 – 2.9** was handed the deleted-action history (gate 2.21) and a
  client negotiating **2.5 – 2.9** was handed the `image_id` body (gate 2.45) — granting more than
  it asked for, which is precisely what this file says it exists to prevent;
- the 406 range check had the same flaw in a third place: `"2.100"` parsed to `2.1`, landed inside
  `[2.1, 2.9]`, and was **accepted** rather than refused.

Found while implementing A2: rendering the parsed float back to a string produced `"compute 2.9"`
for a client that asked for `2.90`. The **reporting** half was fixed then, by echoing the request's
string instead of re-rendering the float (`appliedMicroversion`) — but comparison stayed float-based,
and no string echo could fix that.

**An earlier revision of this entry deferred the repair and closed with "every gate in this product
sits below 2.9 and is unaffected". That sentence was false, and it is why the defect shipped.** The
gates are 2.21 and 2.45: below 2.9 as floats, above it as versions — so they were precisely the
gates affected. The trigger condition it named ("any follow-up that adds a gate *above* 2.9") could
never fire, because the problem was never about gates above 2.9. Recorded here rather than quietly
corrected, because the mitigation reasoning is the part worth not repeating: **a claim that a
numeric defect is out of reach has to be checked against the actual thresholds, not against the
value that made the defect visible.**

**Fixed.** `microversion` is now a `struct{ major, minor int }` compared through `atLeast`, and
`parseMicroversion` is the only thing that reads a version out of a string. Both gates, the range
check and the applied-version report share one header reader (`negotiatedVersionString`), so they
cannot disagree about what the client asked for. Pinned by `TestMicroversionOrderingIsNotFloat`
(every minor from 2.2 to 2.90 across both thresholds) and `TestMicroversionRangeCheckIsNotFloat`
(including `2.100`). The previous tests exercised 2.1, 2.21, 2.44, 2.45, 2.90 and `latest` — which
is exactly the set that behaves identically under both orderings, and is why nothing caught it.

---

## Emitted fields checked against upstream's introduction version

The enumeration FR-042 asks for: what the product's compute serializers actually emit, and when
upstream introduced each. Fields the product does **not** emit are out of scope by FR-042 and are
listed only where their absence explains a decision.

| Field the product emits | Upstream introduced | Gated here? | Note |
|---|---|---|---|
| `id`, `name`, `status`, `tenant_id`, `user_id`, `created`, `updated` | 2.1 | n/a | in the base representation; nothing to gate |
| `addresses` | 2.1 | n/a | shape unchanged across the range |
| `OS-EXT-STS:power_state` | 2.1 | n/a | extension merged into 2.1 |
| `OS-SRV-USG:launched_at` | 2.1 | n/a | extension merged into 2.1 |
| `security_groups` | 2.1 | n/a | extension merged into 2.1 |
| `key_name` | 2.1 | n/a | present in the base representation |
| `image_id` (createBackup action body) | **2.45** | ✅ **yes** (A1) | returned unconditionally before |
| `image_id` (createImage action body) | **2.45** | ✅ **yes** (A7) | never returned before; found on the host |
| `fault` | 2.1 | no | object shape stable; its `code` semantics shifted without a version boundary |
| `flavor` as an embedded object with `vcpus`/`ram`/`disk` | **2.47** | ❌ **no** | see below |
| `locked` | **2.9** | ❌ **no** | not emitted by Lightstack's serializers; out of scope by FR-042 |
| `description`, `tags`, `host_status`, `hostname` | 2.19 / 2.26 / 2.16 / 2.90 | n/a | not emitted |

### The `flavor` shape (2.47) — found, left ungated, and why

Upstream changed the server representation's `flavor` at **2.47**: before it, `flavor` is a reference
(`{"id": ..., "links": [...]}`); from 2.47 the flavor's details are embedded in the server. Lightstack emits
the embedded form — `{"id", "vcpus", "ram", "disk"}` — at every version, so a client negotiating 2.1
receives a 2.47 shape.

**Left ungated in this story**, recorded rather than skipped:

- It is a genuine FR-039 case and should be gated. It is not fixed here because the pre-2.47 form
  requires `links` on the flavor reference, which means producing flavor self-links inside the server
  serializer — a self-link surface SPEC-025 rewrote product-wide, and one this story would be
  reaching back into for a shape no current Lightstack client asks for.
- A4 caps the exposure: only a client that negotiates **explicitly** below 2.47 sees the wrong
  shape, and Lightstack's own runbooks and contract suites pin nothing.
- Doing it properly needs the flavor-links work and its own verification, which is a task, not a
  line.

**Consequence, stated plainly**: FR-039 is satisfied for `image_id` and not for `flavor`. The
advertised range remains partly aspirational in a third way, alongside A4. (A6 was the fourth; it is
now fixed.)

---

## What a follow-up should pick up

1. ~~**A6 first** (integer comparison)~~ — **done.** Any new gate can now be expressed at its real
   version, including one between 2.9 and 2.90.
2. **The 2.47 `flavor` shape**, together with flavor self-links in the server serializer.
3. **A4** only as a deliberate, announced compatibility break, never as a refactor.
