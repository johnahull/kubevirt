# VEP-300 Managed DRA Claims — implementation handoff

> **Delete this file before opening the PR.** It exists only to hand work between machines.

Branch: `feature/vep-300-managed-dra-claims` on `johnahull/kubevirt`, forked from `main` @ `f79595e9c2`.

VEP: https://github.com/johnahull/enhancements/blob/vep-300-managed-dra-claims/veps/sig-compute/300-managed-dra-claims/vep.md

## Why this handoff exists

The first machine could not run `make generate` (docker socket permission denied) and links Go test
binaries pathologically slowly — the `virt-api/.../admitters` test binary took **40 minutes** in the
external linker (`-extld=gcc`, forced by cgo in the dependency graph), versus <1s for other packages.
Everything cheap to verify was verified there; everything expensive was written but not run.

**Work in strict TDD**: every behaviour below was written test-first with a verified RED before the
implementation. Continue that way.

---

## Verification status — read this before trusting anything

| Area | Package | Status |
|---|---|---|
| `ManagedClaimName`, pod rendering, metadata routing | `pkg/dra` | ✅ 37 specs green |
| Validation rules 1, 2, 3, 5, 7 | `pkg/dra/admitter` | ✅ 55 specs green |
| Device collection, request/config building, aligner | `pkg/managed-claim`, `.../aligner` | ✅ green (before the reconciler was added) |
| Launcher pod rendering caller | `pkg/virt-controller/services` | ✅ green |
| Rule 6 — `ManagedClaimProvisioner` admitter | `pkg/virt-api/.../admitters` | ✅ 9 specs green |
| Rule 4 — immutability | `pkg/virt-api/.../admitters` | ✅ verified green |
| Reconciler | `pkg/managed-claim` | ✅ compiles and green |
| `ManagedClaimsReady` aggregation | `pkg/virt-controller/watch/vmi` | ✅ 7 specs green |
| Codegen (Step 0) | whole tree | ✅ make generate clean; whole tree builds |

### First thing to do on the fast machine

```bash
go test ./pkg/managed-claim/... ./pkg/dra/... \
        ./pkg/virt-api/webhooks/validating-webhook/admitters/ -count=1
```

Two known-unverified items:

1. **`pkg/managed-claim/reconcile_test.go` has never compiled successfully.** It was written, the RED
   was confirmed (undefined `Reconciler`/`NewReconciler`/labels), `reconcile.go` was written, and then
   every run timed out in the linker. Expect small compile fixes. It uses
   `k8s.io/client-go/kubernetes/fake`, which is vendored.
2. **Rule 4's specs were rewritten after a real failure and not rerun.** Original version called
   `admitHotplug` with `api.NewMinimalVMI`, which panicked: `admitHotplugCPU`
   (`pkg/virt-api/webhooks/validating-webhook/admitters/vmi-update-admitter.go:237`) dereferences
   `oldCPUTopology.MaxSockets` with **no nil guard**, and the minimal VMI has `Spec.Domain.CPU == nil`.
   Rewritten to test `admitManagedClaimsImmutable` directly plus one wiring spec through
   `admitHotplug` with a populated CPU topology.

> **Pre-existing bug, deliberately not fixed:** that nil-deref in `admitHotplugCPU` is unreachable in
> production (the mutating webhook defaults CPU first), so fixing it is out of scope for VEP-300.
> Worth a separate upstream issue.

---

## What is done

### Commit `24de8c0ec4` — API types, feature gate, codegen wiring
- New `staging/src/kubevirt.io/api/core/v1alpha1/` with `ManagedClaimProvisioner`,
  `ManagedClaimProvisionerSpec`, `ManagedClaimDeviceType`, plus resource-name constants.
- `ManagedClaimProvisionerName *string` on `VirtualMachineInstanceResourceClaim` (`core/v1/types.go`).
- `VirtualMachineInstanceManagedClaimProvisioningFailed` condition type (later replaced by the
  single-writer `ManagedClaimsReady` condition — see Step 3).
- `ManagedDRAClaims` gate: const + `RegisterFeatureGate` in `pkg/virt-config/featuregate/active.go`,
  accessor `ManagedDRAClaimsEnabled()` in `pkg/virt-config/feature-gates.go`.
- All five generators wired in `hack/generate.sh` (swagger-doc, deepcopy-gen, **openapi-gen**,
  client-gen `--input`, controller-gen crd).
- `deepcopy_generated.go` for `core/v1alpha1` and `core/v1` generated natively via
  `go run k8s.io/code-generator/cmd/deepcopy-gen@v0.34.3` because docker was unavailable.

### Commit `85d8b59679` — naming, pod rendering, metadata routing
- `pkg/dra/managed_claim.go`: `ManagedClaimName(vmiName, claimName)` and `IsManagedClaim`.
- `ToPodResourceClaims` now takes the VMI name and emits the derived name for managed entries.
- **Bug fix**: `resolveClaimMetadata` (`pkg/dra/utils.go`) routed managed claims to
  `resourceclaimtemplates/` because it branched on `ResourceClaimName != nil`, which is nil for a
  managed claim — while kubelet writes under `resourceclaims/<derived-name>/`. Affected GPU, generic
  host devices, **and** SR-IOV. `vmiName` threaded through `GetPCIAddressForClaim`,
  `GetMDevUUIDForClaim`, and their three launcher callers.

### Commit `a42279b3c4` — VMI spec validation (rules 1, 2, 3, 5, 7)
In `pkg/dra/admitter/`. Rule 3 lives in `managed_claim_provisioner.go` behind a narrow
`ProvisionerGetter` interface.

### Commit `a191138da9` — framework and aligner
`pkg/managed-claim/` (collection, `BuildRequests`, `BuildConfigs`, `RequestNamesFor`) and
`pkg/managed-claim/aligner/` (`policy.kubevirt.io/aligner`).

### Uncommitted at handoff → see final commit on the branch
Rule 4 immutability, rule 6 CRD admitter, and the reconciler.

---

## Design decisions worth preserving

These are load-bearing; a reviewer or refactor could undo them by accident.

1. **`ManagedClaimName` is a frozen wire contract.** virt-controller renders it into the launcher pod
   while the provisioner controller names the ResourceClaim; virt-operator rolls virt-controller out
   *first* (`pkg/virt-operator/resource/apply/update.go`). Changing the truncation or hash algorithm
   silently breaks claim binding mid-upgrade. sha256, first 8 hex chars, 253-char limit.
2. **`BuildRequests` emits in fixed `cpu/gpu/hostDevice/network` order.** Requests come from a map;
   Go's randomized iteration would otherwise reorder them each reconcile and the converge logic would
   rewrite the claim forever — a hot loop against the apiserver.
3. **Rule 3 fails admission only on a definitive `NotFound`.** Any other lookup error is swallowed:
   an unhealthy informer must not block all VMI creation.
4. **Rule 5 (device coverage) applies only to managed entries.** Direct and template claims are
   user-authored and may carry requests this VMI doesn't consume. There is a passing test pinning this.
5. **The aligner skips the PCIe constraint for a single device** — nothing to align against, and it's
   what makes the VEP's single-GPU example come out with empty `constraints`.
6. **CPUs align on NUMA, not PCIe root.** A CPU group is affine to several PCIe roots, so the CPU DRA
   driver publishes `pcieRoot` as a list (KEP-5491) while GPU/NIC drivers publish a scalar. The NUMA
   constraint is emitted with **no** request list so it covers the whole claim.
7. **`cpu` is inert.** `collectCPU` in `pkg/managed-claim/collect.go` is the single edit point for
   VEP-152. `pkg/managed-claim/provisioner.go` has a placeholder `CPUDRASource`; core `v1.CPU` was
   deliberately **not** touched so this branch stays purely VEP-300 and independently upstreamable.
8. **Rule 3 is a closure `SpecValidator`, not a threaded lister.** `VMICreateAdmitter.SpecValidators`
   is variadic and `registerValidatingWebhooks(informers *webhooks.Informers)` (`pkg/virt-api/api.go:990`)
   already holds the informers — SIG-Network and SIG-Storage plug in the same way at `api.go:992-999`.
   Threading a lister through `ValidateVirtualMachineInstanceSpec` would have hit six call sites.

---

## Remaining work

### Step 0 — unblock codegen (prerequisite for steps 2 and 3) — DONE

Codegen ran with rootless **podman** (no docker needed). KubeVirt auto-selects the
runtime (`hack/common.sh` tries `podman ps` first). The one snag on rootless podman is
that `hack/dockerized` syncs the tree with `rsync -al`, and preserving owner/group fails
inside the user namespace (`chown ... Invalid argument`); adding `--no-o --no-g` to the
`_rsync` helper fixes it. That edit is an environment workaround, not part of the feature:
it is held with `git update-index --skip-worktree hack/dockerized` and must not be committed.

Two `make generate` failures were real work, both now committed:
- `hack/bootstrap-ginkgo.sh` keys off the `*_suite_test.go` filename, so the aligner
  package (its `TestAligner` lived in `aligner_test.go`) tripped auto-generation. Fixed by
  giving it a conventional suite file.
- `ManagedClaimProvisionerList.Items` has no `listType` marker (like every other `*List`),
  so it was added to `api/api-rule-violations-known.list`.

Generated and committed: `types_swagger_generated.go` (core/v1 and core/v1alpha1),
`openapi_generated.go`, swagger.json, the **kubevirt v1alpha1 typed clientset + fake**,
scheme/clientset registration, `components/validations_generated.go`, apitesting golden
data, and `BUILD.bazel` files. Generated output is committed separately from hand-written code.

**No listers/informers were generated, and none will be** — KubeVirt runs no
lister-gen/informer-gen (see `hack/generate.sh`). Informers are hand-built in
`pkg/controller/virtinformers.go` with `cache.NewSharedIndexInformer` over a ListWatch on
the clientset. This changes Step 2 (below).

> **Open question to settle before investing in codegen:** the VEP specifies
> `apiVersion: kubevirt.io/v1alpha1`, which is the **core** group — and core currently serves only
> `v1`. Every other alpha type in KubeVirt lives in a subgroup (`pool.kubevirt.io`,
> `plugin.kubevirt.io`). It is mechanically fine (`pool/v1alpha1` and `pool/v1beta1` already coexist),
> but a reviewer will stop on it. The fallback — a `managedclaim.kubevirt.io` subgroup — is a
> directory rename plus the six codegen touch points, the CRD `Group` field, and the RBAC
> `APIGroups` strings. **Cheap now, expensive after the clientsets exist.** Raise it on the VEP
> tracking issue first.

### Step 1 — verify what's already written

Run the test command at the top of this file. Fix the reconciler test compile errors and rule 4.

### Step 2 — controller binary (`cmd/managed-claim-controller/`)

Model on `cmd/synchronization-controller/synchronization-controller.go`: `pkg/service`, leader
election via `pkg/virt-controller/leaderelectionconfig`, `kubecli`, healthz. **Simpler in one
respect** — it serves no endpoint, so it needs no TLS cert bootstrap.

Wire it as:
```go
managedclaim.NewReconciler(
    aligner.ProvisionerName,
    &aligner.Provisioner{},
    k8sClient,                 // kubernetes.Interface
    provisionerStore,          // implements managedclaim.ProvisionerStore
)
```

The only genuinely new piece is a `ProvisionerStore`. There is **no generated lister**
(see Step 0), so back it with the ManagedClaimProvisioner informer's indexer. A
`cache.NewGenericLister(informer.GetIndexer(), corev1alpha1.Resource("managedclaimprovisioners"))`
returns a real `apierrors` NotFound, which Rule 3 relies on:

```go
type listerStore struct{ lister cache.GenericLister }

func (s *listerStore) Get(name string) (*corev1alpha1.ManagedClaimProvisioner, error) {
    obj, err := s.lister.Get(name)   // NotFound APIStatus error when absent
    if err != nil {
        return nil, err
    }
    return obj.(*corev1alpha1.ManagedClaimProvisioner), nil
}
```

Build the informer itself in `pkg/controller/virtinformers.go` alongside the others, with a
ListWatch on `kubevirt.KubevirtV1alpha1().ManagedClaimProvisioners()`.

Still needed around it: VMI + ManagedClaimProvisioner informers, a workqueue, and expectations
(reuse `pkg/controller/expectations.go` — do not write new machinery). The VEP asks the framework to
provide expectations to provisioner controllers.

Also wire the image build, mirroring `synchronization-controller`:
`hack/bazel-build-images.sh`, `hack/bazel-push-images.sh`, root `BUILD.bazel`,
`tools/manifest-templator/manifest-templator.go`, `tools/csv-generator/csv-generator.go`.

### Step 3 — `ManagedClaimsReady` condition (single-writer)

**Design constraint:** VMI status has a single writer, virt-controller, which persists it via a
full-object `Update` (`pkg/virt-controller/watch/vmi/vmi.go`). A provisioner controller must **never**
patch `virtualmachineinstances/status`; doing so races virt-controller's Update and violates the
invariant. So provisioning state is surfaced the same way DataVolume readiness is
(`aggregateDataVolumesConditions`): the provisioner controller owns only the ResourceClaims and emits
**events** for generation failures, and virt-controller mirrors the owned ResourceClaims onto the VMI.

Done in this branch:
- `core/v1/types.go` defines `VirtualMachineInstanceManagedClaimsReady` plus the
  `...ReasonAllManagedClaimsReady` / `...ReasonNotAllManagedClaimsReady` reasons (the old
  provisioner-written `ManagedClaimProvisioningFailed` condition was removed).
- `pkg/virt-controller/watch/vmi/managedclaims.go` implements
  `aggregateManagedClaimsConditions(vmiCopy, claims)`: True once every managed entry has a
  ResourceClaim with `.status.allocation` set, False otherwise. Unit-tested in `managedclaims_test.go`.

Remaining (integration, not yet wired): call `aggregateManagedClaimsConditions` from
`updateStatus` in `lifecycle.go` next to `aggregateDataVolumesConditions` (~line 307), fed by a new
ResourceClaim informer/indexer on the `vmi.Controller` (add a `ResourceClaim()` informer to
`pkg/controller/virtinformers.go`, a `NewController` param, an enqueue-owning-VMI event handler, and a
namespace+owner list in the sync path), all gated on `ManagedDRAClaims`. This uses the vendored
`k8s.io/api/resource/v1` client — **no codegen** — but is best landed alongside Step 2 (the producer).

### Step 4 — virt-operator wiring

- **CRD**: `NewManagedClaimProvisionerCrd()` in `components/crds.go`, modelled on
  `NewMigrationPolicyCrd` (line 884) or `NewVirtualMachineClusterInstancetypeCrd` (796) — both are
  single-version and cluster-scoped, using the typed constant `extv1.ClusterScoped`. **Not**
  `NewVirtualMachinePoolCrd`: namespaced, dual-version, scale-subresource baggage. Register at
  `install/strategy.go:499`.
- **Gated deployment**: copy the `DecentralizedLiveMigration` pattern that gates
  `virt-synchronization-controller`: `apply/reconcile.go:398` and `:616`, `apply/update.go:55`,
  `apply/core.go:817` (lease cleanup), plus `NewManagedClaimControllerDeployment` in
  `components/deployments.go` near `VirtSynchronizationControllerName` (line 50).
- **RBAC — two roles:**
  - New `rbac/managedclaimcontroller.go` modelled on `rbac/synchronizationcontroller.go:32`:
    `create/get/list/watch/update/delete` on `resourceclaims` in `resource.k8s.io`,
    `get/list/watch` on `virtualmachineinstances` and `managedclaimprovisioners`, and `create/patch`
    on `events`. **No** access to `virtualmachineinstances/status` — the provisioner never writes VMI
    status (see Step 3). Splice its `GetAll…(namespace)` into the `rbaclist` in
    `install/strategy.go` — a separate step from CRD/Deployment registration.
  - **virt-api** needs `get/list/watch` on `managedclaimprovisioners` in `rbac/apiserver.go:41`,
    or the Rule 3 closure validator 403s at admission.

### Step 5 — register the two new webhook paths

- Rule 3 closure `SpecValidator` at `pkg/virt-api/api.go:992`, alongside the SIG-Network and
  SIG-Storage ones, capturing a `ProvisionerGetter`. Needs a new field on the fixed
  `webhooks.Informers` struct (`pkg/virt-api/webhooks/utils.go:66`) plus bootstrap wiring.
- `ManagedClaimProvisionerAdmitter` (already written) registered in
  `validating-webhook.go` and `components/webhooks.go`.

### Step 6 — e2e and examples

- Ginkgo e2e for the gated deployment and the CRD, following the feature-gate-driven patterns in
  `tests/decorators/decorators.go`, `tests/testsuite/kubevirtresource.go`, `tests/operator/operator.go`.
- `examples/` manifests for the VEP's three worked cases.
- Cluster smoke test: `make cluster-up && make cluster-sync`, enable `ManagedDRAClaims`, then confirm
  the CRD is established; the controller Deployment exists **only** while the gate is on; applying the
  `pcie-aligned` provisioner + GPU/NIC VMI yields ResourceClaim `gpu-nic-vm-aligned-devices` with the
  expected requests, constraints, labels, and VMI owner reference; the launcher pod's
  `.spec.resourceClaims[].resourceClaimName` matches; deleting the claim re-creates it; deleting the
  VMI garbage-collects it.
- Real-driver e2e needs the R760xa/XE8640 with the DRA drivers from
  `dra-topology-aware-co-placement/charts/`. That is the VEP's **Beta** requirement, not Alpha.

---

## Alpha limitations to document rather than fix

- **A down provisioner controller is indistinguishable from a slow one.**
  `ManagedClaimsReady` is aggregated from ResourceClaims that exist. If the Deployment is unavailable
  or hasn't won leader election, no claim appears, so the condition simply stays `False`
  (NotAllManagedClaimsReady) and the pod sits `Pending` — the same signal as a claim still being
  allocated. Rule 3 catches typos, not liveness. The VEP acknowledges this.
- **Rolling-upgrade name skew** — see design decision 1.
- **Rule 3 does not cover the VM/VMIRS creation paths.** They call
  `ValidateVirtualMachineInstanceSpec` directly, which has no cluster access. A bad reference there
  surfaces when the VMI is created. Accepted: it's defense-in-depth, not correctness.

## Out of scope

`cpu` execution (VEP-152), live migration of DRA devices, guest NUMA construction (VEP-115),
partition mode, multiple DeviceClasses per device type.
