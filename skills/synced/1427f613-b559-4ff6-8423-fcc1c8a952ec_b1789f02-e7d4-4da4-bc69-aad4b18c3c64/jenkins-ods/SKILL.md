---
name: jenkins-ods
description: Jenkins/ODS pipeline diagnosis: build failures, SonarQube, AquaSec, ImageStream, stageBuild, odsComponentPipeline.
version: 1.0.0
---

# Jenkins ODS Pipeline Helper

## Auth

Credentials live in `~/.jenkins-creds` (sourced at shell startup, never committed):

```bash
export JENKINS_URL=https://jenkins.your-bi-domain.com
export JENKINS_USER=<service-account>
export JENKINS_TOKEN=<api-token>
```

Verify access: `curl -su "$JENKINS_USER:$JENKINS_TOKEN" "$JENKINS_URL/api/json" | jq .mode`

## Quick Commands

| Task | Command |
|------|---------|
| Latest build status | `curl -su "$JENKINS_USER:$JENKINS_TOKEN" "$JENKINS_URL/job/<folder>/job/<job>/lastBuild/api/json" \| jq '{result,timestamp,duration}'` |
| Console log (tail) | `curl -su "$JENKINS_USER:$JENKINS_TOKEN" "$JENKINS_URL/job/<folder>/job/<job>/lastBuild/consoleText" \| tail -200` |
| Build parameters | `curl -su "$JENKINS_USER:$JENKINS_TOKEN" "$JENKINS_URL/job/<folder>/job/<job>/lastBuild/api/json" \| jq '.actions[] \| select(._class=="hudson.model.ParametersAction") \| .parameters'` |
| Trigger rebuild | `curl -su "$JENKINS_USER:$JENKINS_TOKEN" -X POST "$JENKINS_URL/job/<folder>/job/<job>/build"` |

## ODS Pipeline Anatomy

`odsComponentPipeline` stages execute in order:

| # | Stage | What It Does | On Failure |
|---|-------|-------------|------------|
| 1 | `odsComponentStageImportOpenShiftImageOrElse` | Pull or build base image | Check registry connectivity, ImageStream tags |
| 2 | `stageBuildOpenShiftImage` | Run BuildConfig, watch ImageStream | Check BuildConfig logs: `oc logs bc/<name>` |
| 3 | `stageScanForSonarqube` | Static analysis + quality gate | Check build artifacts for `report.json`; review quality gate config |
| 4 | `stageScanForAquaSec` | Container vulnerability scan | CVE report in build artifacts; check severity thresholds |
| 5 | `stageDeployToOpenShift` | DeploymentConfig rollout | Check DC events: `oc describe dc/<name>` |

## Failure Pattern Quick-Ref

| Error Pattern | Likely Cause | Fix |
|--------------|-------------|-----|
| `oc rsync` errors | SA token expired | Re-auth: `oc login` or rotate SA token in Jenkins credentials |
| `ImageStream not found` | Missing project mapping | Check `ods-configuration` ConfigMap for correct namespace/project |
| `quality gate failed` | SonarQube thresholds breached | Review `report.json` in artifacts; fix code smells or override gate |
| `AquaSec scan FAILED` | Critical CVE in base image | Check CVE report; update base image or add exception |
| `Timeout waiting for build` | BuildConfig hung | Check build pod logs: `oc logs -f bc/<name>`; check resource quotas |
| `rollout failed` | Pod crash or readiness probe | `oc get events --sort-by=.lastTimestamp`; check pod logs |

## Diagnostic Workflow

When a pipeline fails:

1. **Get build result**: fetch `lastBuild/api/json`, check `.result` field
2. **Identify failed stage**: search console log for `FAILURE` or `ERROR`
3. **Match pattern**: compare error against the failure table above
4. **Get stage-specific logs**: use the "On Failure" column for the matching stage
5. **Check ODS config**: `oc get cm ods-configuration -o yaml` for project mappings

## Rationalization Prevention

| Rationalization | Why It's Wrong |
|----------------|---------------|
| "I'll just re-trigger the build" | Flaky failures mask real issues; diagnose first |
| "The error is obvious from the stage name" | ODS stages have non-obvious dependencies; check the failure table |
| "SonarQube failures are just warnings" | Quality gates block deployment; they're hard failures |
| "I don't need to check the console log" | API JSON `.result` alone doesn't show which stage failed |
