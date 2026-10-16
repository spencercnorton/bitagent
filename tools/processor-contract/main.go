// Generate the structural JSON Schema and portable examples from the public Go
// wire types. Semantic rules (bindings, stage/proposal consistency, bounds) are
// normative in processorapi/v1/validate.go and exercised by conformance tests.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

var defs = map[string]any{}

func shape(t reflect.Type) map[string]any {
	if t == reflect.TypeOf(json.RawMessage{}) {
		return map[string]any{"type": "object"}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return map[string]any{"anyOf": []any{shape(t.Elem()), map[string]any{"type": "null"}}}
	case reflect.Struct:
		if _, ok := defs[t.Name()]; !ok {
			defs[t.Name()] = nil
			properties := map[string]any{}
			required := []string{}
			var fields func(reflect.Type)
			fields = func(t reflect.Type) {
				for n := 0; n < t.NumField(); n++ {
					f := t.Field(n)
					if f.Anonymous {
						fields(f.Type)
						continue
					}
					tag := strings.Split(f.Tag.Get("json"), ",")
					if tag[0] == "" || tag[0] == "-" {
						continue
					}
					properties[tag[0]] = shape(f.Type)
					if !strings.Contains(f.Tag.Get("json"), "omitempty") && f.Type.Kind() != reflect.Pointer {
						required = append(required, tag[0])
					}
				}
			}
			fields(t)
			if t.Name() == "Telemetry" || t.Name() == "ServiceStatus" {
				required = []string{}
				if t.Name() == "ServiceStatus" {
					required = []string{"contract", "processor", "state"}
				}
			}
			defs[t.Name()] = map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
		}
		return map[string]any{"$ref": "#/$defs/" + t.Name()}
	case reflect.Slice:
		return map[string]any{"type": []string{"array", "null"}, "items": shape(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": []string{"object", "null"}, "additionalProperties": shape(t.Elem())}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Interface:
		return map[string]any{"type": "null"}
	}
	panic(t.String())
}
func write(path string, v any) {
	raw, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		panic(e)
	}
	if e = os.MkdirAll(filepath.Dir(path), 0755); e != nil {
		panic(e)
	}
	if e = os.WriteFile(path, append(raw, '\n'), 0644); e != nil {
		panic(e)
	}
}
func main() {
	root := "docs/processors"
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	for _, v := range []any{v1.Manifest{}, v1.Job{}, v1.Result{}, v1.ServiceStatus{}, v1.Status{}, v1.RegistryWrite{}, v1.RegistryControl{}, v1.JobPage{}} {
		shape(reflect.TypeOf(v))
	}
	for name, fields := range map[string]map[string]any{
		"Identity":      {"id": map[string]any{"type": "string", "pattern": "^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,95}$"}},
		"Job":           {"contract": map[string]any{"const": v1.Contract}, "stage": map[string]any{"enum": v1.Stages}},
		"Result":        {"contract": map[string]any{"const": v1.Contract}, "stage": map[string]any{"enum": v1.Stages}, "outcome": map[string]any{"enum": []string{"completed", "abstained", "deferred", "failed", "excluded", "unmatched"}}},
		"Manifest":      {"contract": map[string]any{"const": v1.Contract}},
		"Deterministic": {"processing_outcome": map[string]any{"enum": []string{"completed", "deferred", "not_attempted"}}, "matching_outcome": map[string]any{"enum": []string{"matched", "completed_unmatched", "deferred", "not_attempted"}}},
	} {
		props := defs[name].(map[string]any)["properties"].(map[string]any)
		for key, value := range fields {
			props[key] = value
		}
	}
	write(filepath.Join(root, "v1.schema.json"), map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": "urn:bitagent:processor:v1", "$defs": defs, "description": "Select a definition for the endpoint. Cross-field validation is normative in the v1 contract."})
	id := v1.Identity{ID: "reference", Version: "1.0.0", ModelVersion: "none", PolicyVersion: "synthetic-v1", ArtifactDigest: "none"}
	j := v1.Job{Binding: v1.Binding{Contract: v1.Contract, JobID: strings.Repeat("1", 64), InputRevision: strings.Repeat("2", 64), PipelineRevision: strings.Repeat("3", 64), ConfigurationDigest: strings.Repeat("4", 64), Processor: id, Stage: v1.Classify}, Configuration: json.RawMessage(`{}`), Deadline: "2035-01-01T00:00:00Z", Input: v1.Input{InfoHash: strings.Repeat("a", 40), Name: "Synthetic.Amber.2024.mkv", Size: 734003200, Files: []v1.File{}, Candidates: []v1.Candidate{}, PriorResults: []v1.Result{}, FilesComplete: true, Deterministic: v1.Deterministic{ProcessingOutcome: "completed", MatchingOutcome: "not_attempted", Title: "Synthetic Amber", Year: 2024}}}
	r := v1.Result{Binding: j.Binding, Outcome: v1.Completed, Proposal: &v1.Proposal{ContentType: "movie"}, Reasons: []string{"synthetic_fixture"}, Evidence: []v1.Evidence{}}
	write(filepath.Join(root, "fixtures", "manifest.json"), v1.Manifest{Contract: v1.Contract, Processor: id, Capabilities: []v1.Stage{v1.Classify}, ConfigurationSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`), Qualification: "synthetic"})
	write(filepath.Join(root, "fixtures", "job.json"), j)
	write(filepath.Join(root, "fixtures", "result.json"), r)
	write(filepath.Join(root, "fixtures", "service-status.json"), v1.ServiceStatus{Contract: v1.Contract, Processor: id, State: "ready", InstanceID: "synthetic-instance", StartedAt: "2026-01-01T00:00:00Z"})
}
