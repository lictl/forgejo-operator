export const meta = {
  name: 'forgejo-new-version',
  description: 'Bring forgejo-operator up to date with a new Forgejo release (branch cut, release day or patch): detect, research, update the version table, verify on a local cluster, review',
  whenToUse: 'When `make forgejo-versions` reports STALE or WARN findings, or a Forgejo major is branched/released or a patch fixes a security issue or advisory. Args: {event?: "branch-cut"|"release"|"patch", major?: number, kubeContext: string, dockerContext?: string, kubeconfig?: string}. kubeContext is the kube context of the local test cluster and is required for every stage that touches a cluster; dockerContext is the docker context of the engine that cluster runs on (omit it when that engine is the current docker context); kubeconfig is the path of the kubeconfig file to create for kubeContext. Without event and major it acts on what the version tool reports. Runs against the local test cluster only and never commits.',
  phases: [
    { title: 'Detect', detail: 'Run the version tool and classify findings', model: 'sonnet' },
    { title: 'Research', detail: 'Release notes, breaking changes, image inspection', model: 'opus' },
    { title: 'Implement', detail: 'Version table, traits, defaults, docs, checklist; deploy to the local cluster', model: 'opus' },
    { title: 'Verify', detail: 'Execute test/e2e/checklists/versions.md', model: 'sonnet' },
    { title: 'Fix', detail: 'Triage and fix failures, then re-run them', model: 'opus' },
    { title: 'Review', detail: 'Adversarial review and fixes', model: 'opus' },
  ],
}

// Process: docs/versions.md "When a new Forgejo version is released". Version data lives in
// internal/forgejo/versions.go; the verification checklist in test/e2e/checklists/versions.md.
const a = args || {}
const KC = a.kubeconfig || '${TMPDIR:-/tmp}/forgejo-operator-local.kubeconfig'
const RULES = 'Work in the repository root (the current project). Never run git commands that change state (no commit, stash, reset, checkout of source files); the person running this workflow commits.'

const DETECT = {
  type: 'object',
  properties: {
    exitCode: { type: 'integer' },
    stale: { type: 'array', items: { type: 'string' } },
    warn: { type: 'array', items: { type: 'string' } },
    info: { type: 'array', items: { type: 'string' } },
    matrix: { type: 'string' },
  },
  required: ['exitCode', 'stale', 'warn', 'info', 'matrix'],
}

phase('Detect')
const detected = await agent(`${RULES} Run \`make forgejo-versions\` (it is \`go run ./hack/forgejo-versions\`; it needs network access but no credentials). Return its exit code (0 = table current, 1 = stale, 2 = upstream unreadable), every line starting with STALE, WARN and INFO (verbatim, without the prefix), and the printed matrix. Do not change anything.`, { label: 'detect', phase: 'Detect', model: 'sonnet', schema: DETECT })

if (!detected) return { status: 'failed', reason: 'version tool did not run' }
if (detected.exitCode === 2) return { status: 'blocked', reason: 'upstream release data unreadable; retry later', detected }
const explicit = a.event || a.major
if (!explicit && detected.stale.length === 0 && detected.warn.length === 0) {
  return { status: 'up-to-date', matrix: detected.matrix, info: detected.info }
}
if (!a.kubeContext) return { status: 'blocked', reason: 'pass args.kubeContext: the kube context of the local test cluster' }

// From here on a stage may touch the local test cluster (args.kubeContext) and the docker engine it
// runs on (args.dockerContext, optional: without it docker uses the current docker context).
const KCTX = a.kubeContext
const DENV = a.dockerContext ? `DOCKER_CONTEXT=${a.dockerContext} ` : ''
const DOCKER_NOTE = a.dockerContext
  ? `Run every docker command, and every make target that runs docker, with \`DOCKER_CONTEXT=${a.dockerContext}\` (\`export DOCKER_CONTEXT=${a.dockerContext}\` in every shell), so that it reaches the docker engine the local cluster runs on.`
  : 'docker commands use the current docker context, which must be the docker engine the local cluster runs on.'
const CLUSTER = `Cluster: the local test cluster only (kube context "${KCTX}"). Create a kubeconfig that holds only that context once with \`kubectl config view --minify --flatten --context "${KCTX}" > ${KC}\` and \`export LOCAL_KUBE_CONTEXT="${KCTX}" OPERATOR_KUBECONFIG=${KC} KUBECONFIG=${KC}\` in every shell that runs kubectl or helm. Never use any other kube context (in particular never the user's real "k3s" cluster). ${DOCKER_NOTE} The operator runs in-cluster in forgejo-operator-system; redeploy a build with \`${DENV}make deploy-local IMG=forgejo-operator:dev && git checkout -- config/manager/kustomization.yaml\`. Pull images into the cluster with \`${DENV}docker pull <image>\`.`

const target = explicit
  ? `Requested: event=${a.event || 'infer from findings'}, major=${a.major || 'infer from findings'}.`
  : 'Act on the STALE and WARN findings below.'
log(`${target} stale=${detected.stale.length} warn=${detected.warn.length}`)

phase('Research')
const research = await agent(`${RULES} Research a new Forgejo release for forgejo-operator, a Kubernetes operator that runs the official image (read docs/design.md, docs/versions.md — especially "Per-major differences" and "When a new Forgejo version is released" — and internal/forgejo/versions.go first).
${target}
Version tool findings: ${JSON.stringify({ stale: detected.stale, warn: detected.warn, info: detected.info })}
Matrix:
${detected.matrix}

Follow step 2 of the branch-cut procedure in docs/versions.md for every new major (release notes "Breaking features"/"Breaking bug fixes", PRs labelled breaking in the milestone, config cheat sheet / CLI reference / upgrade guide diff against the previous major, Dockerfiles, /api/healthz, /api/v1/version, /metrics, security announcements), and for patches check the advisory and security notes. Inspect the images hands-on with \`${DENV}docker\` (for a branch cut: codeberg.org/forgejo-experimental/forgejo:N.0-test and -rootless; otherwise codeberg.org/forgejo/forgejo:N and -rootless): binary path, uid 1000 user git, the bootstrap CLI (migrate, admin user list/create/change-password), generate secret, and a run of "forgejo web --config <app.ini>" as uid 1000 with a read-only root filesystem against PostgreSQL. Report each operator-relevant difference with a source URL and the trait or table change it needs. Do not edit repository files.`, { label: 'research', phase: 'Research', model: 'opus' })

phase('Implement')
const impl = await agent(`${RULES} Update forgejo-operator for the new Forgejo release, following docs/versions.md "When a new Forgejo version is released" exactly (branch cut / release day / patch / end of life, whichever applies).
${target}
Version tool findings: ${JSON.stringify({ stale: detected.stale, warn: detected.warn })}
Research:
${research}

Do: the table row(s) and advisories in internal/forgejo/versions.go; new traits with their rendering in internal/core and unit tests (forgejo.Versions.WithVerified(N)); for a new LTS the default image, the +kubebuilder:default marker and \`make manifests generate\`, samples, README and docs examples; the support matrix and calendar in docs/versions.md; the test expectations named in the procedure. Update test/e2e/checklists/versions.md: its version facts (image tags, versions, dates, which majors are Supported/Deprecated/EndOfLife) and checks for the new major (fresh install root and rootless, upgrade from the current LTS and from the previous release, refused downgrade, backup and restore). Keep \`go build ./... && go vet ./... && go test ./internal/... -run 'Test[^C]'\` green and \`make forgejo-versions\` free of STALE lines that this change should resolve.
Then deploy for verification. ${CLUSTER} Pull every image the checklist uses.
Report (<= 350 words): what changed and why, anything you could not resolve.`, { label: 'implement', phase: 'Implement', model: 'opus' })

const RESULT = {
  type: 'object',
  properties: {
    checks: { type: 'array', items: { type: 'object', properties: {
      id: { type: 'string' }, passed: { type: 'boolean' }, evidence: { type: 'string' },
    }, required: ['id', 'passed', 'evidence'] } },
    notes: { type: 'string' },
  },
  required: ['checks'],
}
const runChecklist = (round, ids) => agent(`${RULES} ${CLUSTER} Do not redeploy the operator. Execute test/e2e/checklists/versions.md exactly as written${ids ? `, but only these checks and the setup steps they depend on: ${ids.join(', ')}; use namespace e2e-versions-r${round} instead of e2e-versions` : ''}. Read its shared facts first. If the namespace already exists, delete it and wait until it is gone. Do not fix failures: record evidence (status, events, operator log lines for your namespace) and continue. Delete the namespace at the end. Report every check id you ran.`, { label: `verify:r${round}`, phase: 'Verify', model: 'sonnet', schema: RESULT })

phase('Verify')
let results = await runChecklist(1)
const allChecks = {}
for (const c of (results && results.checks) || []) allChecks[c.id] = c
const rounds = []
for (let round = 2; round <= 3; round++) {
  const failed = Object.values(allChecks).filter(c => !c.passed)
  if (!failed.length) break
  phase('Fix')
  const fix = await agent(`${RULES} Version checks failed for forgejo-operator (checklist test/e2e/checklists/versions.md; process docs/versions.md). ${CLUSTER}
Failed checks:
${JSON.stringify(failed, null, 2)}
Diagnose each (code, live objects, operator logs). For an operator bug: fix it, keep build/vet/tests green, and redeploy. For a wrong check: correct test/e2e/checklists/versions.md and explain. Report (<= 250 words) per check id: root cause, bug or checklist issue, change made.`, { label: `fix:r${round}`, phase: 'Fix', model: 'opus' })
  phase('Verify')
  const rerun = await runChecklist(round, failed.map(c => c.id))
  for (const c of (rerun && rerun.checks) || []) allChecks[c.id] = c
  rounds.push({ round, failed: failed.map(c => c.id), fix })
}

phase('Review')
const REVIEW = {
  type: 'object',
  properties: { issues: { type: 'array', items: { type: 'object', properties: {
    title: { type: 'string' }, severity: { type: 'string', enum: ['high', 'medium', 'low'] }, file: { type: 'string' }, scenario: { type: 'string' }, fix: { type: 'string' },
  }, required: ['title', 'severity', 'file', 'scenario', 'fix'] } } },
  required: ['issues'],
}
const review = await agent(`${RULES} Adversarially review the uncommitted change (\`git diff\` and untracked files from \`git status\`) that updates forgejo-operator for a new Forgejo release. Implementer's report:
${impl}
Check against docs/versions.md and the research: existing instances must not change (stored images, rendered app.ini, pod templates for unaffected majors); the support classification and upgrade gate around the new major and its dates; traits that could break a still-supported major; default image only moved for a new LTS; docs and checklist consistent with the code. Report only concrete, reachable defects. Do not edit files or touch any cluster.`, { label: 'review', phase: 'Review', model: 'opus', schema: REVIEW })
const real = ((review && review.issues) || []).filter(i => i.severity !== 'low')
let reviewFix = 'No medium/high review issues.'
if (real.length) {
  reviewFix = await agent(`${RULES} Fix these review findings in the uncommitted Forgejo version update (explain instead of changing code when a finding is wrong). Keep build/vet/tests green. If runtime behavior changes: ${CLUSTER} redeploy and re-run the affected checks of test/e2e/checklists/versions.md in namespace e2e-versions-review, then delete it.
Findings:
${JSON.stringify(real, null, 2)}
Report (<= 250 words).`, { label: 'review-fix', phase: 'Review', model: 'opus' })
}

const checks = Object.values(allChecks)
return {
  status: checks.every(c => c.passed) ? 'verified' : 'needs-attention',
  passed: checks.filter(c => c.passed).length,
  total: checks.length,
  failed: checks.filter(c => !c.passed).map(c => ({ id: c.id, evidence: c.evidence.slice(0, 500) })),
  implementation: impl,
  rounds,
  review: review && review.issues,
  reviewFix,
  next: 'Review the diff, run `make forgejo-versions`, then commit.',
}
