package llmeval

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const checkedInOpenModelBakeoffRegistry = "open-model-bakeoff-2026-07-28.json"

func TestCheckedInOpenModelBakeoffRegistryValid(t *testing.T) {
	registry, manifest, manifestSHA256 := loadCheckedInBakeoffRegistry(t)
	if err := ValidateOpenModelBakeoffRegistry(registry, manifest, manifestSHA256); err != nil {
		t.Fatalf("ValidateOpenModelBakeoffRegistry() error = %v", err)
	}
	if got := len(OpenModelBakeoffRequiredEvidenceSHA256(registry)); got == 0 {
		t.Fatal("OpenModelBakeoffRequiredEvidenceSHA256() returned no identities")
	}
	if got := len(registry.NotRun); got != 12 {
		t.Fatalf("not_run count = %d, want 12", got)
	}
}

func TestOpenModelBakeoffRegistryRejectsUnsafeOrUnboundEvidence(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*OpenModelBakeoffRegistry)
		wantErr string
	}{
		{
			name: "source material",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				registry.SourceSafety.SourceMaterialIncluded = true
			},
			wantErr: "source_safety",
		},
		{
			name: "manifest digest",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				registry.Bindings.Manifest.SHA256 = strings.Repeat("0", 64)
			},
			wantErr: "does not match",
		},
		{
			name: "artifact path",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				for task, cells := range registry.Cells {
					cells[0].Artifact.Basename = "../private/result.json"
					registry.Cells[task] = cells
					break
				}
			},
			wantErr: "artifact basename",
		},
		{
			name: "evaluator coverage",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				for name, build := range registry.Bindings.EvaluatorBuilds {
					if len(build.CellIDs) < 2 {
						continue
					}
					build.CellIDs = build.CellIDs[1:]
					registry.Bindings.EvaluatorBuilds[name] = build
					break
				}
			},
			wantErr: "coverage is incomplete",
		},
		{
			name: "cost source",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				for task, cells := range registry.Cells {
					cells[0].Cost.CostSource = "provider_observed"
					registry.Cells[task] = cells
					break
				}
			},
			wantErr: "unsupported cost_source",
		},
		{
			name: "cross-task corpus",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				cells := registry.Cells[TaskMatcherExtract]
				cells[0].CorpusBinding = "contentfilter_historical_gold"
				registry.Cells[TaskMatcherExtract] = cells
			},
			wantErr: "corpus_binding task does not match",
		},
		{
			name: "cross-task selection",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				cells := registry.Cells[TaskMatcherExtract]
				cells[0].ComparisonSelection = "junk_scale_160"
				registry.Cells[TaskMatcherExtract] = cells
			},
			wantErr: "comparison_selection task does not match",
		},
		{
			name: "incomplete manifest matrix",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				registry.NotRun = registry.NotRun[1:]
			},
			wantErr: "system/task coverage is incomplete",
		},
		{
			name: "production observation after registry",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				registry.CurrentProduction.AsOfUTC = "2026-07-30"
			},
			wantErr: "cannot be later than produced_utc",
		},
		{
			name: "not-run overlaps a cell",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				cell := registry.Cells[TaskContentFilter][0]
				registry.NotRun[0].SystemID = cell.SystemID
				registry.NotRun[0].Task = TaskContentFilter
				registry.NotRun[0].Lane = cell.Lane
			},
			wantErr: "both cells and not_run",
		},
		{
			name: "legacy manifest requires diagnostic registry policy",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				registry.EvidencePolicy.AllHistoricalScreens = "formal"
			},
			wantErr: "explicitly diagnostic, non-promotion registry",
		},
		{
			name: "legacy cell requires diagnostic decision",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				cells := registry.Cells[TaskContentFilter]
				for i := range cells {
					if cells[i].SystemID ==
						"or-nano-azure-content-shadow-20260728" {
						cells[i].Assessment.Decision = "pass"
						break
					}
				}
				registry.Cells[TaskContentFilter] = cells
			},
			wantErr: "explicitly diagnostic and non-promotion",
		},
		{
			name: "legacy preflight requires diagnostic decision",
			mutate: func(registry *OpenModelBakeoffRegistry) {
				for i := range registry.PreflightIncompatibilities {
					if registry.PreflightIncompatibilities[i].SystemID ==
						"or-nova-micro-amazon-bedrock-content-shadow-feasibility-v2-20260728" {
						registry.PreflightIncompatibilities[i].Decision =
							"incompatible"
						break
					}
				}
			},
			wantErr: "explicitly diagnostic and non-promotion",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry, manifest, manifestSHA256 := loadCheckedInBakeoffRegistry(t)
			test.mutate(&registry)
			err := ValidateOpenModelBakeoffRegistry(registry, manifest, manifestSHA256)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ValidateOpenModelBakeoffRegistry() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestReadOpenModelBakeoffRegistryRejectsUnknownAndDuplicateFields(t *testing.T) {
	registry, _, _ := loadCheckedInBakeoffRegistry(t)
	raw, err := json.Marshal(registry)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	unknown := bytes.Replace(raw, []byte(`"status":`), []byte(`"unexpected":true,"status":`), 1)
	if _, err := ReadOpenModelBakeoffRegistry(bytes.NewReader(unknown)); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}

	duplicate := bytes.Replace(raw, []byte(`"status":`), []byte(`"schema_version":1,"status":`), 1)
	if _, err := ReadOpenModelBakeoffRegistry(bytes.NewReader(duplicate)); err == nil ||
		!strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate field error = %v", err)
	}
}

func TestReadOpenModelBakeoffRegistryRejectsSourceBearingMaterial(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*OpenModelBakeoffRegistry) []byte
		wantErr string
	}{
		{
			name: "forbidden source field",
			mutate: func(registry *OpenModelBakeoffRegistry) []byte {
				raw, err := json.Marshal(registry)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				return bytes.Replace(raw, []byte(`{`), []byte(`{"prompt":"private",`), 1)
			},
			wantErr: "forbidden source-bearing field",
		},
		{
			name: "unix absolute path",
			mutate: func(registry *OpenModelBakeoffRegistry) []byte {
				registry.Purpose = "/private/evidence.json"
				raw, err := json.Marshal(registry)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				return raw
			},
			wantErr: "absolute filesystem path",
		},
		{
			name: "windows UNC path",
			mutate: func(registry *OpenModelBakeoffRegistry) []byte {
				registry.Purpose = `\\server\private\evidence.json`
				raw, err := json.Marshal(registry)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				return raw
			},
			wantErr: "absolute filesystem path",
		},
		{
			name: "credential-like text",
			mutate: func(registry *OpenModelBakeoffRegistry) []byte {
				registry.Purpose = "Authorization: Bearer redacted"
				raw, err := json.Marshal(registry)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				return raw
			},
			wantErr: "credential-like material",
		},
		{
			name: "multiline free text",
			mutate: func(registry *OpenModelBakeoffRegistry) []byte {
				registry.Purpose = "line one\nline two"
				raw, err := json.Marshal(registry)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				return raw
			},
			wantErr: "multiline",
		},
		{
			name: "gitlab token value",
			mutate: func(registry *OpenModelBakeoffRegistry) []byte {
				registry.Purpose = "glpat-1234567890abcdef"
				raw, err := json.Marshal(registry)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				return raw
			},
			wantErr: "credential-like material",
		},
		{
			name: "github token map key",
			mutate: func(registry *OpenModelBakeoffRegistry) []byte {
				registry.TaskCaveats["ghp_1234567890abcdef"] = []string{"redacted"}
				raw, err := json.Marshal(registry)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				return raw
			},
			wantErr: "unsafe map key",
		},
		{
			name: "AWS access key value",
			mutate: func(registry *OpenModelBakeoffRegistry) []byte {
				registry.Purpose = "redacted " + "AKIA" + "IOSFODNN7EXAMPLE redacted"
				raw, err := json.Marshal(registry)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				return raw
			},
			wantErr: "credential-like material",
		},
		{
			name: "PEM private key marker",
			mutate: func(registry *OpenModelBakeoffRegistry) []byte {
				registry.Purpose = "-----BEGIN " + "PRIVATE KEY-----"
				raw, err := json.Marshal(registry)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				return raw
			},
			wantErr: "credential-like material",
		},
		{
			name: "sensitive arbitrary map field",
			mutate: func(registry *OpenModelBakeoffRegistry) []byte {
				registry.TaskCaveats["aws_secret_access_key"] = []string{"redacted"}
				raw, err := json.Marshal(registry)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				return raw
			},
			wantErr: "forbidden source-bearing field",
		},
		{
			name: "database password arbitrary map key",
			mutate: func(registry *OpenModelBakeoffRegistry) []byte {
				registry.TaskCaveats["database_password"] = []string{"not-sensitive-looking"}
				raw, err := json.Marshal(registry)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				return raw
			},
			wantErr: "unsafe map key",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, _, _ := loadCheckedInBakeoffRegistry(t)
			raw := test.mutate(&candidate)
			_, err := ReadOpenModelBakeoffRegistry(bytes.NewReader(raw))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ReadOpenModelBakeoffRegistry() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestReadOpenModelBakeoffRegistryRejectsCredentialLikeDynamicMapKeyVariants(t *testing.T) {
	tests := []string{
		"database_password_backup",
		"db-password-v2",
		"apiTokenEncrypted",
		"client.secret.value",
		"private-key-pem",
		"awsAccessKeyID",
		"httpAuthorizationHeader",
		"bearer_credentials",
		"readReplicaDSN",
		"secretkey",
		"secretKeyBackup",
		"secret-key-v2",
		"passphrase",
		"dbPassphraseBackup",
		"db-passphrase-v2",
		"signingkey",
		"signingKeyPEM",
		"signing-key-v2",
		"encryptionkeyBackup",
		"hmac-key-material",
		"sharedsecret",
		"consumersecret",
		"signingsecret",
		"sharedsecretbackup",
		"consumersecretvalue",
		"masterkey",
		"serviceaccountkey",
		"jwttoken",
		"sastoken",
		"saskey",
		"awscredentials",
		"cloudcredentials",
		"logincredentials",
		"authheader",
		"sessioncookie",
		"dbpass",
		"smtppass",
		"csrftoken",
		"connectionstring",
		"databaseurl",
		"dbpwd",
	}

	for _, key := range tests {
		t.Run(key, func(t *testing.T) {
			registry, _, _ := loadCheckedInBakeoffRegistry(t)
			registry.TaskCaveats[key] = []string{"ordinary value"}
			raw, err := json.Marshal(registry)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			_, err = ReadOpenModelBakeoffRegistry(bytes.NewReader(raw))
			if err == nil || !strings.Contains(err.Error(), "unsafe map key") {
				t.Fatalf("ReadOpenModelBakeoffRegistry() error = %v, want unsafe map key", err)
			}
		})
	}
}

func TestCredentialLikeBakeoffRegistryMapKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{key: "secretkey", want: true},
		{key: "secretKeyBackup", want: true},
		{key: "secret-key-v2", want: true},
		{key: "passphrase", want: true},
		{key: "dbPassphraseBackup", want: true},
		{key: "db-passphrase-v2", want: true},
		{key: "signingkey", want: true},
		{key: "signingKeyPEM", want: true},
		{key: "signing-key-v2", want: true},
		{key: "encryptionkeyBackup", want: true},
		{key: "hmac-key-material", want: true},
		{key: "sharedsecret", want: true},
		{key: "consumersecret", want: true},
		{key: "signingsecret", want: true},
		{key: "sharedsecretbackup", want: true},
		{key: "consumersecretvalue", want: true},
		{key: "masterkey", want: true},
		{key: "master-key-v2", want: true},
		{key: "serviceaccountkey", want: true},
		{key: "serviceAccountKeyID", want: true},
		{key: "service-account-key-v2", want: true},
		{key: "jwttoken", want: true},
		{key: "sastoken", want: true},
		{key: "saskey", want: true},
		{key: "awscredentials", want: true},
		{key: "cloudcredentials", want: true},
		{key: "logincredentials", want: true},
		{key: "authheader", want: true},
		{key: "sessioncookie", want: true},
		{key: "dbpass", want: true},
		{key: "smtppass", want: true},
		{key: "csrftoken", want: true},
		{key: "connectionstring", want: true},
		{key: "dbConnectionStringValue", want: true},
		{key: "databaseurl", want: true},
		{key: "postgres-uri", want: true},
		{key: "dbpwd", want: true},
		{key: "cost_source", want: false},
		{key: "source_artifacts", want: false},
		{key: "secretary", want: false},
		{key: "public_key_algorithm", want: false},
		{key: "publickeyalgorithm", want: false},
		{key: "monkey", want: false},
		{key: "keyboard_layout", want: false},
		{key: "compass", want: false},
		{key: "passage", want: false},
		{key: "tokenization", want: false},
		{key: "header_count", want: false},
		{key: "cookie_policy", want: false},
		{key: "documentation_url", want: false},
		{key: "masterKeyboard", want: false},
		{key: "adsNetwork", want: false},
	}

	for _, test := range tests {
		t.Run(test.key, func(t *testing.T) {
			if got := credentialLikeBakeoffRegistryMapKey(test.key); got != test.want {
				t.Fatalf("credentialLikeBakeoffRegistryMapKey(%q) = %t, want %t", test.key, got, test.want)
			}
		})
	}
}

func TestReadOpenModelBakeoffRegistryAllowsDeclaredSourceMetadataFields(t *testing.T) {
	registry, _, _ := loadCheckedInBakeoffRegistry(t)
	raw, err := json.Marshal(registry)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if _, err := ReadOpenModelBakeoffRegistry(bytes.NewReader(raw)); err != nil {
		t.Fatalf("ReadOpenModelBakeoffRegistry() error = %v", err)
	}

	if credentialLikeBakeoffRegistryMapKey("cost_source") {
		t.Fatal("cost_source must not be treated as a credential-like key")
	}
	if credentialLikeBakeoffRegistryMapKey("source_artifacts") {
		t.Fatal("source_artifacts must not be treated as a credential-like key")
	}
}

func loadCheckedInBakeoffRegistry(
	t *testing.T,
) (OpenModelBakeoffRegistry, SystemManifest, string) {
	t.Helper()
	registryPath := opsFixture(t, filepath.Join("..", "..", "ops", "llm-eval", checkedInOpenModelBakeoffRegistry))
	registryFile, err := os.Open(registryPath)
	if err != nil {
		t.Fatalf("os.Open(registry) error = %v", err)
	}
	registry, readErr := ReadOpenModelBakeoffRegistry(registryFile)
	closeErr := registryFile.Close()
	if readErr != nil {
		t.Fatalf("ReadOpenModelBakeoffRegistry() error = %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("registry Close() error = %v", closeErr)
	}

	manifestPath := filepath.Join("..", "..", "ops", "llm-eval", registry.Bindings.Manifest.Basename)
	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("os.ReadFile(manifest) error = %v", err)
	}
	manifest, digest, err :=
		DecodeOpenModelBakeoffRegistrySystemManifest(manifestRaw)
	if err != nil {
		t.Fatalf(
			"DecodeOpenModelBakeoffRegistrySystemManifest() error = %v",
			err,
		)
	}
	return registry, manifest, digest
}
