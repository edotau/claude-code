---
name: openshift
description: BI-specific OpenShift: manifests, troubleshooting, S2I builds, ODS CI/CD, Nexus registry, and operator runbooks (diagnose, rollback, promote, drift) for digitalbtd-insilico-suite.
---

# OpenShift

BI OpenShift operations: BI-specific infrastructure context plus operator runbooks for multi-step remediation.

## Routing

| Task | Reference |
|------|-----------|
| Create/edit K8s/OCP manifests | `references/manifests.md` |
| Debug pod failures, CrashLoopBackOff, OOM | `references/troubleshooting.md` |
| Build and deploy containers | `references/deploy.md` |
| S2I builds with BuildConfig/ImageStream | `references/s2i.md` |
| Authenticate to OpenShift clusters | `references/auth.md` |

## BI Infrastructure

| Component | Setup |
|-----------|-------|
| CI/CD | ODS Jenkins pipelines |
| Registry | Nexus (`nexus.internal:8443`) — not Docker Hub |
| Auth | Azure AD SSO → `oc login` with service account tokens |
| Templates | `openshift/` directory in this repo |
| Environments | dev / qa / prod namespaces |

```
openshift/
├── deployment/   builds/    services/   configmaps/   secrets/   pipelines/
docker/
├── Dockerfile    docker-compose.yml   (local dev only — NOT production)
```

## Common Operations

```bash
# Deploy to dev
make build

# Pod health
oc get pods -n btd-portal-dev
oc logs -f deploy/btd-portal --tail=100

# Debug CrashLoopBackOff
oc logs <pod> --previous --tail=200
oc describe pod <pod>
oc debug deploy/btd-portal

# Auth
oc login --token=$(cat /var/run/secrets/token) --server=https://api.ocp.internal:6443
```

## BI Gotchas

- `docker-compose.yml` is local dev only — production uses `openshift/` templates
- Config drift between `docker/` and `openshift/` is recurring — verify both
- Nexus auth required: `oc create secret docker-registry nexus-pull ...`
- Azure AD tokens expire after 1h — re-login or `oc whoami -t`
- ODS pipelines auto-tag with git SHA — manual tags need `oc tag` explicitly

---

## Operator Runbooks

Before any runbook, verify context: `oc whoami 2>/dev/null && oc project -q`

### Mode 1: Diagnose — Failing Pod

Walk this funnel in order; stop at first root cause:

1. `oc get pod <name> -o wide` + `oc get events --field-selector involvedObject.name=<name>`
2. `oc logs <name> --tail=200` then `oc logs <name> --previous --tail=200`
3. `oc adm top pod <name>` — check OOMKilled in `oc describe`
4. `oc describe pod <name> | grep -A2 Mounts` — verify `/Volumes/` paths
5. Diff live manifest vs `chart/` (see Mode 4)
6. `oc get pod <name> -o jsonpath='{.status.containerStatuses[*].imageID}'`

### Mode 2: Rollback

```bash
oc rollout history deployment/<name>                    # review revisions
oc rollout history deployment/<name> --revision=<N>     # inspect specific
# CONFIRM WITH USER before running:
oc rollout undo deployment/<name> --to-revision=<N>
oc rollout status deployment/<name> --timeout=5m
```

### Mode 3: Promote

```bash
./scripts/ocp_image_manager.sh --env <dev|qa|prod> --action build,push,deploy --tag-with-commit
```

| Env | Gate | Confirm |
|-----|------|---------|
| dev | `make test-smoke` green | Build freely |
| qa | `make test-unit` + PR merged to `dev` | Confirm SHA |
| prod | `make test-all` + QA sign-off | **Require explicit approval** |

### Mode 4: Drift Check

```bash
helm template insilico-suite ./chart --values chart/values-<env>.yaml > /tmp/desired.yaml
oc get deployment <name> -o yaml \
  | yq 'del(.metadata.managedFields, .metadata.resourceVersion, .status, .metadata.uid)' \
  > /tmp/live.yaml
diff -u /tmp/desired.yaml /tmp/live.yaml | head -100
```

Diffs: **benign** (timestamps, resourceVersion) vs **config drift** (env vars, image tag) vs **structural drift** (volumes, security context — requires chart update).

## Safety Rules

- Confirm before: `oc delete`, `oc rollout undo`, any push to qa/prod
- Never `oc apply -f` without diffing first (Mode 4)
- Never modify `scripts/*.sh` during an incident — propose fix after containment
