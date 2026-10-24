/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1_test

// These tests run the generated CRDs (config/crd/bases) through the API server's own CRD
// validation, defaulting, OpenAPI schema validation and CEL validation, without a cluster:
// the rules must compile within the CEL cost budget, the samples must pass, and the mistakes
// the rules are there for must be refused with their messages.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/robfig/cron/v3"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// crdSchema is one generated CRD, ready to default and validate objects like the API server.
type crdSchema struct {
	crd        *apiextensions.CustomResourceDefinition
	props      *apiextensions.JSONSchemaProps
	structural *structuralschema.Structural
}

func loadCRD(t *testing.T, plural string) *crdSchema {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "forgejo.lucas.icu_"+plural+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	v1 := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.UnmarshalStrict(data, v1); err != nil {
		t.Fatal(err)
	}
	crd := &apiextensions.CustomResourceDefinition{}
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(v1, crd, nil); err != nil {
		t.Fatal(err)
	}
	crd.Status.StoredVersions = []string{"v1alpha1"}
	props := crd.Spec.Validation
	if props == nil {
		props = crd.Spec.Versions[0].Schema
	}
	if props == nil || props.OpenAPIV3Schema == nil {
		t.Fatalf("%s: no schema", plural)
	}
	s, err := structuralschema.NewStructural(props.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	return &crdSchema{crd: crd, props: props.OpenAPIV3Schema, structural: s}
}

// validate defaults obj (and old) and returns the errors the API server would answer a create
// (old == "") or an update with.
func (c *crdSchema) validate(t *testing.T, obj, old string) field.ErrorList {
	t.Helper()
	parse := func(s string) map[string]any {
		// Decoded as the API server does: whole numbers become int64.
		j, err := yaml.YAMLToJSON([]byte(s))
		if err != nil {
			t.Fatalf("bad test object: %v\n%s", err, s)
		}
		m := map[string]any{}
		if err := utiljson.Unmarshal(j, &m); err != nil {
			t.Fatalf("bad test object: %v\n%s", err, s)
		}
		structuraldefaulting.Default(m, c.structural)
		return m
	}
	o := parse(obj)
	var oldObj any
	if old != "" {
		oldObj = parse(old)
	}
	sv, _, err := schemavalidation.NewSchemaValidator(c.props)
	if err != nil {
		t.Fatal(err)
	}
	errs := schemavalidation.ValidateCustomResource(nil, o, sv)
	if v := cel.NewValidator(c.structural, true, celconfig.PerCallLimit); v != nil {
		celErrs, _ := v.Validate(context.Background(), nil, c.structural, o, oldObj, celconfig.RuntimeCELCostBudget)
		errs = append(errs, celErrs...)
	}
	return errs
}

type validationCase struct {
	name string
	obj  string
	old  string
	// want is a substring of the expected error; empty means the object is valid.
	want string
}

func runCases(t *testing.T, plural string, cases []validationCase) {
	t.Helper()
	c := loadCRD(t, plural)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := c.validate(t, tc.obj, tc.old)
			if tc.want == "" {
				if len(errs) > 0 {
					t.Fatalf("want valid, got %v", errs.ToAggregate())
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("want an error containing %q, got none", tc.want)
			}
			if got := errs.ToAggregate().Error(); !strings.Contains(got, tc.want) {
				t.Fatalf("want an error containing %q, got %s", tc.want, got)
			}
		})
	}
}

func TestCRDsPassAPIServerValidation(t *testing.T) {
	for _, plural := range []string{"forgejos", "forgejobackups", "forgejorestores", "forgejorunners"} {
		t.Run(plural, func(t *testing.T) {
			c := loadCRD(t, plural)
			// Compiles every CEL rule and checks it against the cost budget, as creating the
			// CRD does.
			if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), c.crd); len(errs) > 0 {
				t.Fatal(errs.ToAggregate())
			}
		})
	}
}

func TestSamplesAreValid(t *testing.T) {
	samples := map[string]string{
		"forgejo_v1alpha1_forgejo.yaml":        "forgejos",
		"forgejo_v1alpha1_forgejobackup.yaml":  "forgejobackups",
		"forgejo_v1alpha1_forgejorestore.yaml": "forgejorestores",
		"forgejo_v1alpha1_forgejorunner.yaml":  "forgejorunners",
	}
	for file, plural := range samples {
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", file))
			if err != nil {
				t.Fatal(err)
			}
			if errs := loadCRD(t, plural).validate(t, string(data), ""); len(errs) > 0 {
				t.Fatal(errs.ToAggregate())
			}
		})
	}
}

// forgejo is a Forgejo named name with the given spec (YAML, indented by two spaces).
func forgejo(name, spec string) string {
	return "apiVersion: forgejo.lucas.icu/v1alpha1\nkind: Forgejo\nmetadata:\n  name: " + name +
		"\n  namespace: ns\nspec:\n" + spec + "\n"
}

func fj(spec string) string { return forgejo("git", spec) }

const errCredentials = "credentialsSecret"

// errSLOTarget is the field path of the availability target's validation errors.
const errSLOTarget = "spec.monitoring.slo.availabilityTarget"

// errPostgresTag is the message of a CNPG image whose tag does not start with a major version.
const errPostgresTag = "starts with the PostgreSQL major version"

// errRunbookBase is the field path of the runbook base URL's validation errors.
const errRunbookBase = "spec.monitoring.runbookBaseURL"

func TestForgejoValidation(t *testing.T) {
	const s3 = `endpoint: https://s3.example.com
      bucket: bucket
      credentialsSecret: {name: s3}`
	runCases(t, "forgejos", []validationCase{
		{name: "defaults", obj: fj("  {}")},

		// Image (L05): an empty image would follow DefaultImage of later operator releases.
		{name: "empty image", obj: fj(`  image: ""`), want: "spec.image"},
		{name: "pull policy", obj: fj(`  imagePullPolicy: Sometimes`), want: "Unsupported value"},
		{name: "empty postgres image", obj: fj("  database:\n    cnpg: {imageName: \"\"}"), want: "spec.database.cnpg.imageName"},
		// CloudNativePG reads the major version from the tag (docs/upgrades.md, "PostgreSQL major upgrades").
		{name: "postgres image with a major tag", obj: fj("  database:\n    cnpg: {imageName: \"ghcr.io/cloudnative-pg/postgresql:18-minimal-trixie\"}")},
		{name: "postgres image with a minor tag and a digest", obj: fj("  database:\n    cnpg: {imageName: \"localhost:5000/pg:18.6-minimal-trixie@sha256:" +
			strings.Repeat("a", 64) + "\"}")},
		{name: "postgres image of the library", obj: fj("  database:\n    cnpg: {imageName: \"postgres:18\"}")},
		{name: "postgres image without a tag", obj: fj("  database:\n    cnpg: {imageName: \"localhost:5000/postgresql\"}"), want: errPostgresTag},
		{name: "postgres image latest", obj: fj("  database:\n    cnpg: {imageName: \"ghcr.io/cloudnative-pg/postgresql:latest\"}"), want: errPostgresTag},
		{name: "postgres image digest only", obj: fj("  database:\n    cnpg: {imageName: \"ghcr.io/cloudnative-pg/postgresql@sha256:" + strings.Repeat("a", 64) + "\"}"),
			want: errPostgresTag},
		{name: "External postgres image digest only", obj: fj("  database:\n    type: External\n    external: {host: pg, credentialsSecret: {name: pg}}\n" +
			"    cnpg: {imageName: \"ghcr.io/cloudnative-pg/postgresql@sha256:" + strings.Repeat("a", 64) + "\"}")},

		// Names.
		{name: "name with a dot", obj: forgejo("my.git", "  {}"), want: "DNS-1035"},
		{name: "name starting with a digit", obj: forgejo("9git", "  {}"), want: "DNS-1035"},
		{name: "name of 47 with CNPG", obj: forgejo(strings.Repeat("a", 47), "  {}")},
		{name: "name of 48 with CNPG", obj: forgejo(strings.Repeat("a", 48), "  {}"), want: "at most 47 characters"},
		{name: "name of 56 with External", obj: forgejo(strings.Repeat("a", 56),
			"  database:\n    type: External\n    external: {host: pg, credentialsSecret: {name: pg}}")},
		{name: "name of 57 with External", obj: forgejo(strings.Repeat("a", 57),
			"  database:\n    type: External\n    external: {host: pg, credentialsSecret: {name: pg}}"), want: "at most 56 characters"},
		{name: "name of 45 with HA", obj: forgejo(strings.Repeat("a", 45), "  highAvailability: {enabled: true}")},
		{name: "name of 46 with HA", obj: forgejo(strings.Repeat("a", 46), "  highAvailability: {enabled: true}"), want: "at most 45 characters"},
		{name: "name of 46 without HA", obj: forgejo(strings.Repeat("a", 46), "  highAvailability: {enabled: false}")},
		{name: "name ending in -valkey", obj: forgejo("git-valkey", "  highAvailability: {enabled: false}"), want: "must not end in '-valkey'"},
		{name: "name ending in -cron", obj: forgejo("git-cron", "  highAvailability: {enabled: true}")},
		{name: "name containing valkey", obj: forgejo("valkey-git", "  highAvailability: {enabled: true}")},

		// Replicas and autoscaling.
		{name: "replicas without HA", obj: fj("  replicas: 2"), want: "replicas greater than 1 require highAvailability.enabled"},
		{name: "replicas with HA", obj: fj("  replicas: 3\n  highAvailability: {enabled: true}")},
		{name: "replicas 0", obj: fj("  replicas: 0")},
		{name: "HPA without HA", obj: fj("  autoscaling:\n    horizontal: {enabled: true}"), want: "autoscaling.horizontal requires highAvailability.enabled"},
		{name: "HPA disabled without HA", obj: fj("  autoscaling:\n    horizontal: {enabled: false, minReplicas: 2}")},
		{name: "HPA with HA", obj: fj("  highAvailability: {enabled: true}\n  autoscaling:\n    horizontal: {enabled: true, minReplicas: 2, maxReplicas: 4}")},
		{name: "HPA min above max", obj: fj("  highAvailability: {enabled: true}\n  autoscaling:\n    horizontal: {enabled: true, minReplicas: 4, maxReplicas: 3}"),
			want: "minReplicas must not be greater than maxReplicas"},
		{name: "HPA max below default min", obj: fj("  highAvailability: {enabled: true}\n  autoscaling:\n    horizontal: {enabled: true, maxReplicas: 1}"),
			want: "minReplicas must not be greater than maxReplicas"},
		{name: "HPA CPU target 0", obj: fj("  autoscaling:\n    horizontal: {targetCPUUtilizationPercentage: 0}"), want: "targetCPUUtilizationPercentage"},
		{name: "vertical bounds", obj: fj("  autoscaling:\n    vertical: {enabled: true, minAllowed: {cpu: 100m, memory: 1Gi}, maxAllowed: {cpu: 2, memory: 4Gi}}")},
		{name: "vertical target above 100", obj: fj("  autoscaling:\n    vertical: {targetUtilizationPercentage: 120}"), want: "targetUtilizationPercentage"},

		// Database.
		{name: "External without external", obj: fj("  database: {type: External}"), want: "type External requires external"},
		{name: "external with CNPG", obj: fj("  database:\n    external: {host: pg, credentialsSecret: {name: pg}}"), want: "external requires type External"},
		{name: "external without credentials", obj: fj("  database:\n    type: External\n    external: {host: pg}"), want: errCredentials},
		{name: "External with CNPG instances", obj: fj("  database:\n    type: External\n    external: {host: pg, credentialsSecret: {name: pg}}\n    cnpg: {instances: 3}"),
			want: "cnpg settings apply to type CNPG only"},
		{name: "External with CNPG parameters", obj: fj("  database:\n    type: External\n    external: {host: pg, credentialsSecret: {name: pg}}\n    cnpg: {parameters: {max_connections: \"200\"}}"),
			want: "cnpg settings apply to type CNPG only"},
		{name: "External with postgres image", obj: fj("  database:\n    type: External\n    external: {host: pg, port: 5433, credentialsSecret: {name: pg}}\n    cnpg: {imageName: \"ghcr.io/cloudnative-pg/postgresql:16\", resources: {}}")},
		{name: "external port 0", obj: fj("  database:\n    type: External\n    external: {host: pg, port: 0, credentialsSecret: {name: pg}}"), want: "spec.database.external.port"},

		// Sizes may be lowered: the operator only grows claims and ignores a lower size, so
		// undoing an increase the StorageClass could not apply stops its warning.
		{name: "data volume grows", old: fj("  storage: {size: 10Gi}"), obj: fj("  storage: {size: 20Gi}")},
		{name: "data volume lowered back", old: fj("  storage: {size: 100Gi}"), obj: fj("  storage: {size: 10Gi}")},
		{name: "data volume removed after growth", old: fj("  storage: {size: 100Gi}"), obj: fj("  {}")},
		{name: "database volume lowered", old: fj("  database:\n    cnpg:\n      storage: {size: 5Gi}"), obj: fj("  database:\n    cnpg:\n      storage: {size: 1Gi}")},
		{name: "switch to External after CNPG growth", old: fj("  database:\n    cnpg:\n      storage: {size: 10Gi}"),
			obj: fj("  database:\n    type: External\n    external: {host: pg, credentialsSecret: {name: pg}}")},
		{name: "backup volume lowered", old: fj("  backup:\n    storage: {size: 20Gi}"), obj: fj("  backup:\n    storage: {size: 10Gi}")},
		{name: "existing backup claim size lowered", old: fj("  backup:\n    storage: {size: 50Gi, existingClaim: backups}"), obj: fj("  backup:\n    storage: {size: 1Gi, existingClaim: backups}")},
		{name: "small volume at creation", obj: fj("  storage: {size: 1Gi}")},

		// Server, Services, Ingress.
		{name: "domain with scheme", obj: fj("  server: {domain: \"https://git.example.com\"}"), want: "spec.server.domain"},
		{name: "rootURL without scheme", obj: fj("  server: {rootURL: git.example.com/}"), want: "spec.server.rootURL"},
		{name: "rootURL", obj: fj("  server: {domain: git.example.com, rootURL: \"https://git.example.com/forge/\"}")},
		{name: "nodePort on ClusterIP", obj: fj("  server:\n    http: {nodePort: 30080}"), want: "nodePort requires type NodePort or LoadBalancer"},
		{name: "nodePort on NodePort", obj: fj("  server:\n    http: {type: NodePort, nodePort: 30080}")},
		{name: "port out of range", obj: fj("  server:\n    ssh:\n      service: {port: 70000}"), want: "spec.server.ssh.service.port"},
		{name: "loadBalancerClass on ClusterIP", obj: fj("  server:\n    ssh:\n      service: {loadBalancerClass: lb}"), want: "loadBalancerClass requires type LoadBalancer"},
		{name: "ingress without host", obj: fj("  ingress: {enabled: true}"), want: "ingress.enabled requires ingress.host"},
		{name: "ingress disabled without host", obj: fj("  ingress: {enabled: false}")},
		{name: "ingress with host", obj: fj("  ingress: {enabled: true, host: git.example.com, path: /git}")},
		{name: "ingress with domain", obj: fj("  server: {domain: git.example.com}\n  ingress: {enabled: true}")},
		{name: "ingress host upper case", obj: fj("  ingress: {enabled: true, host: Git.example.com}"), want: "spec.ingress.host"},
		{name: "ingress path //", obj: fj("  ingress: {enabled: true, host: git.example.com, path: /a//b}"), want: "must not contain '//'"},
		{name: "ingress path /.", obj: fj("  ingress: {enabled: true, host: git.example.com, path: /a/.}"), want: "must not end with '/.' or '/..'"},

		// Gateway API (L14): the rules of HTTPPathMatch.
		{name: "gateway path /git/", obj: fj("  gateway:\n    parentRefs: [{name: eg}]\n    path: /git/")},
		{name: "gateway path escapes", obj: fj("  gateway:\n    parentRefs: [{name: eg}]\n    path: /a%20b/~c")},
		{name: "gateway path //", obj: fj("  gateway:\n    parentRefs: [{name: eg}]\n    path: /a//b"), want: "must not contain '//'"},
		{name: "gateway path /./", obj: fj("  gateway:\n    parentRefs: [{name: eg}]\n    path: /a/./b"), want: "must not contain '/./'"},
		{name: "gateway path /./ at the end", obj: fj("  gateway:\n    parentRefs: [{name: eg}]\n    path: /a/./"), want: "must not contain '/./'"},
		{name: "gateway path /../", obj: fj("  gateway:\n    parentRefs: [{name: eg}]\n    path: /a/../b"), want: "must not contain '/../'"},
		{name: "gateway path /..", obj: fj("  gateway:\n    parentRefs: [{name: eg}]\n    path: /a/.."), want: "must not end with '/..'"},
		{name: "gateway path /.", obj: fj("  gateway:\n    parentRefs: [{name: eg}]\n    path: /a/."), want: "must not end with '/.'"},
		{name: "gateway path %2F", obj: fj("  gateway:\n    parentRefs: [{name: eg}]\n    path: /a/%2F"), want: "must not contain '%2F'"},
		{name: "gateway path %2f", obj: fj("  gateway:\n    parentRefs: [{name: eg}]\n    path: /a%2fb"), want: "must not contain '%2F'"},
		{name: "gateway path bad escape", obj: fj("  gateway:\n    parentRefs: [{name: eg}]\n    path: /a/%zz"), want: "spec.gateway.path"},
		{name: "gateway path relative", obj: fj("  gateway:\n    parentRefs: [{name: eg}]\n    path: git"), want: "spec.gateway.path"},
		{name: "gateway without parentRefs", obj: fj("  gateway:\n    parentRefs: []"), want: "spec.gateway.parentRefs"},
		{name: "gateway ssh without SSH", obj: fj("  server:\n    ssh: {enabled: false}\n  gateway:\n    parentRefs: [{name: eg}]\n    ssh:\n      parentRefs: [{name: eg, sectionName: ssh}]"),
			want: "gateway.ssh requires server.ssh.enabled"},
		{name: "ingress with gateway host names", obj: fj("  ingress: {enabled: true}\n  gateway:\n    parentRefs: [{name: eg}]\n    hostnames: [git.example.com]")},

		// TLS.
		{name: "issuer kind", obj: fj("  tls:\n    certManager:\n      issuerRef: {name: ca, kind: ClusterIssuer}")},
		{name: "issuer kind typo", obj: fj("  tls:\n    certManager:\n      issuerRef: {name: ca, kind: ClusterIsuer}"), want: "kind must be Issuer or ClusterIssuer"},
		{name: "external issuer", obj: fj("  tls:\n    certManager:\n      issuerRef: {name: ca, kind: AWSPCAClusterIssuer, group: awspca.cert-manager.io}")},

		// Admin.
		{name: "admin named admin", obj: fj("  admin: {username: Admin}"), want: "is a name Forgejo reserves"},
		{name: "admin name with two dots", obj: fj("  admin: {username: a..b}"), want: "must not contain two of"},
		{name: "admin name with a leading dash", obj: fj("  admin: {username: -root}"), want: "spec.admin.username"},
		{name: "admin name", obj: fj("  admin: {username: site.admin, email: ops@example.com}")},
		{name: "admin email", obj: fj("  admin: {email: ops}"), want: "spec.admin.email"},

		// Backups.
		{name: "schedule with seconds", obj: fj("  backup: {enabled: true, schedule: \"0 0 3 * * *\"}"), want: "must be a cron expression with five fields"},
		{name: "schedule descriptor", obj: fj("  backup: {enabled: true, schedule: \"@every 6h\"}")},
		{name: "schedule with zone", obj: fj("  backup: {enabled: true, schedule: \"CRON_TZ=Asia/Tokyo 30 2 * * 1-5\"}")},
		{name: "remote without credentials", obj: fj("  backup:\n    remote:\n      s3: {endpoint: \"https://s3.example.com\", bucket: bucket}"), want: errCredentials},
		{name: "remote with a generated key", obj: fj("  backup:\n    remote:\n      s3:\n        " + strings.ReplaceAll(s3, "\n      ", "\n        "))},

		// Monitoring: SLOs and the runbook base.
		{name: "slo", obj: fj("  monitoring:\n    slo: {availabilityTarget: \"99.95\", window: 28d}\n    runbookBaseURL: https://wiki.example.com/forgejo/runbooks")},
		{name: "slo target as a number", obj: fj("  monitoring:\n    slo: {availabilityTarget: 99.9}"), want: errSLOTarget},
		{name: "slo target 100", obj: fj("  monitoring:\n    slo: {availabilityTarget: \"100\"}"), want: errSLOTarget},
		{name: "slo target 0.5", obj: fj("  monitoring:\n    slo: {availabilityTarget: \"0.5\"}"), want: errSLOTarget},
		{name: "slo window in minutes", obj: fj("  monitoring:\n    slo: {window: 90m}"), want: "spec.monitoring.slo.window"},
		{name: "slo window compound", obj: fj("  monitoring:\n    slo: {window: 1d12h}"), want: "spec.monitoring.slo.window"},
		{name: "runbook base with a query", obj: fj("  monitoring: {runbookBaseURL: \"https://wiki.example.com/r?page=1\"}"), want: errRunbookBase},
		{name: "runbook base relative", obj: fj("  monitoring: {runbookBaseURL: docs/runbooks}"), want: errRunbookBase},
		{name: "runbook base with a template", obj: fj("  monitoring: {runbookBaseURL: 'https://wiki.example.com/{{'}"), want: errRunbookBase},
		{name: "runbook base with a quote", obj: fj("  monitoring: {runbookBaseURL: 'https://wiki.example.com/a\"b'}"), want: errRunbookBase},

		// Object storage.
		{name: "object storage without credentials", obj: fj("  objectStorage:\n    s3: {endpoint: \"https://s3.example.com\", bucket: bucket}"), want: errCredentials},
		{name: "object storage", obj: fj("  objectStorage:\n    s3:\n      " + s3)},

		// restoreFrom only at creation.
		{name: "restoreFrom at creation", obj: fj("  restoreFrom:\n    remote:\n      path: ns/git/b-1\n      s3:\n        " + strings.ReplaceAll(s3, "\n      ", "\n        "))},
		{name: "restoreFrom added later", old: fj("  {}"), obj: fj("  restoreFrom:\n    remote:\n      path: ns/git/b-1\n      s3:\n        " + strings.ReplaceAll(s3, "\n      ", "\n        ")),
			want: "restoreFrom can only be set when the Forgejo is created"},
	})
}

func backup(spec string) string {
	return "apiVersion: forgejo.lucas.icu/v1alpha1\nkind: ForgejoBackup\nmetadata: {name: b, namespace: ns}\nspec:\n" + spec + "\n"
}

func TestForgejoBackupValidation(t *testing.T) {
	runCases(t, "forgejobackups", []validationCase{
		{name: "backup", obj: backup("  forgejoRef: {name: git}")},
		{name: "ref with a dot", obj: backup("  forgejoRef: {name: my.git}"), want: "spec.forgejoRef.name"},
		{name: "immutable", old: backup("  forgejoRef: {name: git}"), obj: backup("  forgejoRef: {name: git}\n  includeData: false"), want: "spec is immutable"},
	})
}

func restore(spec string) string {
	return "apiVersion: forgejo.lucas.icu/v1alpha1\nkind: ForgejoRestore\nmetadata: {name: r, namespace: ns}\nspec:\n" + spec + "\n"
}

func TestForgejoRestoreValidation(t *testing.T) {
	runCases(t, "forgejorestores", []validationCase{
		{name: "restore", obj: restore("  forgejoRef: {name: git}\n  backupName: b\n  restoreData: false")},
		{name: "nothing to restore", obj: restore("  forgejoRef: {name: git}\n  backupName: b\n  restoreDatabase: false\n  restoreData: false"),
			want: "nothing would be restored"},
		{name: "no source", obj: restore("  forgejoRef: {name: git}"), want: "set exactly one of backupName and remote"},
	})
}

func runner(name, spec string) string {
	return "apiVersion: forgejo.lucas.icu/v1alpha1\nkind: ForgejoRunner\nmetadata: {name: " + name + ", namespace: ns}\nspec:\n" + spec + "\n"
}

func TestForgejoRunnerValidation(t *testing.T) {
	const ref = "  forgejoRef: {name: git}\n"
	runCases(t, "forgejorunners", []validationCase{
		{name: "docker", obj: runner("ci", ref+"  executor: Docker\n  docker: {mountSocket: true, resources: {}}\n  host: {}")},
		{name: "host", obj: runner("ci", ref+"  executor: Host\n  host: {image: node:22}\n  docker: {mountSocket: false, resources: {}}")},
		{name: "no executor", obj: runner("ci", ref), want: "spec.executor"},
		// L25: StatefulSet pods carry controller-revision-hash=<name>-<hash of up to 10>.
		{name: "name of 52", obj: runner(strings.Repeat("r", 52), ref+"  executor: Host")},
		{name: "name of 53", obj: runner(strings.Repeat("r", 53), ref+"  executor: Host"), want: "at most 52 characters"},
		{name: "name with a dot", obj: runner("ci.a", ref+"  executor: Host"), want: "host names"},
		{name: "name ending in -valkey", obj: runner("git-valkey", ref+"  executor: Host"), want: "must not end in '-valkey' or '-cron'"},
		{name: "name ending in -cron", obj: runner("git-cron", ref+"  executor: Host"), want: "must not end in '-valkey' or '-cron'"},
		{name: "name containing cron", obj: runner("cron-ci", ref+"  executor: Host")},
		{name: "organization and repository", obj: runner("ci", ref+"  executor: Host\n  scope: {organization: acme, repository: acme/app}"),
			want: "set at most one of organization and repository"},
		{name: "repository without owner", obj: runner("ci", ref+"  executor: Host\n  scope: {repository: app}"), want: "spec.scope.repository"},
		{name: "socket with Host", obj: runner("ci", ref+"  executor: Host\n  docker: {mountSocket: true}"), want: "docker settings apply to the Docker executor only"},
		{name: "host image with Docker", obj: runner("ci", ref+"  executor: Docker\n  host: {image: node:22}"), want: "host.image applies to the Host executor only"},
		{name: "label of the other executor", obj: runner("ci", ref+"  executor: Host\n  labels: [\"docker:docker://node:22\"]"), want: "labels of the Host executor"},
		{name: "shutdown timeout", obj: runner("ci", ref+"  executor: Host\n  shutdownTimeout: 0s"), want: "positive Go duration"},
		{name: "forgejoRef immutable", old: runner("ci", ref+"  executor: Host"), obj: runner("ci", "  forgejoRef: {name: other}\n  executor: Host"), want: "forgejoRef is immutable"},
	})
}

// TestBackupSchedulePattern keeps the regular expression of the rule on spec.backup.schedule in
// line with the parser of the backup step (cron.ParseStandard): what it accepts must parse (values out of range
// aside, which the operator reports as BackupHealthy=False/InvalidSchedule), and the forms it
// refuses do not parse, except for surrounding white space, which it refuses as well.
func TestBackupSchedulePattern(t *testing.T) {
	c := loadCRD(t, "forgejos")
	rules := c.structural.Properties["spec"].Properties["backup"].Properties["schedule"].XValidations
	if len(rules) != 1 {
		t.Fatalf("want one rule on spec.backup.schedule, got %d", len(rules))
	}
	m := regexp.MustCompile(`^self\.matches\('(.*)'\)$`).FindStringSubmatch(rules[0].Rule)
	if m == nil {
		t.Fatalf("unexpected rule %q", rules[0].Rule)
	}
	re := regexp.MustCompile(m[1])
	for _, tc := range []struct {
		schedule        string
		matches, parses bool
	}{
		{"0 3 * * *", true, true},
		{"*/15 0-6 1,15 * MON-FRI", true, true},
		{"30  2 * *\t1", true, true},
		{"@daily", true, true},
		{"@midnight", true, true},
		{"@every 1h30m", true, true},
		{"CRON_TZ=Asia/Tokyo 0 3 * * *", true, true},
		{"TZ=UTC @hourly", true, true},
		{"0 0 3 * * *", false, false},
		{"0 3 * *", false, false},
		{"@every", false, false},
		{"@every 6", false, false},
		{"@every  6h", false, false},
		{"@nightly", false, false},
		{"@foo 1 2 3 4", false, false},
		{"", false, false},
		{" 0 3 * * *", false, true},
		{"0 3 * * * ", false, true},
	} {
		_, err := cron.ParseStandard(tc.schedule)
		if (err == nil) != tc.parses {
			t.Errorf("cron.ParseStandard(%q): error %v, want parses=%v", tc.schedule, err, tc.parses)
		}
		if re.MatchString(tc.schedule) != tc.matches {
			t.Errorf("pattern matches %q: %v, want %v", tc.schedule, !tc.matches, tc.matches)
		}
	}
}
