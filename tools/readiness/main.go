// readiness creates local blinded diagnostic review packets. It has no database,
// network, provider or runtime-enforcement entry point.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spencercnorton/bitagent/internal/llmreadiness"
)

func read(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open input")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil || len(raw) > 16<<20 {
		return nil, fmt.Errorf("input exceeds size limit")
	}
	return raw, nil
}

func write(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("output exists or cannot be created")
	}
	defer f.Close()
	if _, err = f.Write(raw); err != nil {
		return err
	}
	return f.Sync()
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: readiness freeze|score -snapshot FILE -out NEW_DIRECTORY")
	}
	fs := flag.NewFlagSet("readiness", flag.ContinueOnError)
	snapshotPath := fs.String("snapshot", "", "immutable snapshot JSON")
	out := fs.String("out", "", "new private output directory")
	a := fs.String("reviewer-a", "reviewer-a", "first independent reviewer")
	b := fs.String("reviewer-b", "reviewer-b", "second independent reviewer")
	labelsA := fs.String("labels-a", "", "first label submission")
	labelsB := fs.String("labels-b", "", "second label submission")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *snapshotPath == "" || *out == "" {
		return fmt.Errorf("snapshot and new output directory required")
	}
	raw, err := read(*snapshotPath)
	if err != nil {
		return err
	}
	s, h, err := llmreadiness.LoadSnapshot(raw)
	if err != nil {
		return err
	}
	var artifacts map[string]any
	switch args[0] {
	case "freeze":
		freeze, pa, pb, err := llmreadiness.Prepare(s, h, *a, *b)
		if err != nil {
			return err
		}
		template := func(id string, cases []llmreadiness.ReviewCase) llmreadiness.Submission {
			sub := llmreadiness.Submission{Schema: "bitagent-readiness-labels-v1", SnapshotSHA256: h, ReviewerID: id, Kind: "agent_diagnostic"}
			for _, c := range cases {
				sub.Labels = append(sub.Labels, llmreadiness.Label{CaseID: c.CaseID, InputSHA256: c.InputSHA256})
			}
			return sub
		}
		artifacts = map[string]any{"freeze.json": freeze, "reviewer-a.json": pa, "reviewer-b.json": pb, "labels-a.template.json": template(*a, pa), "labels-b.template.json": template(*b, pb)}
	case "score":
		var sa, sb llmreadiness.Submission
		for _, item := range []struct {
			path string
			sub  *llmreadiness.Submission
		}{{*labelsA, &sa}, {*labelsB, &sb}} {
			if item.path == "" {
				return fmt.Errorf("both independent submissions required")
			}
			data, err := read(item.path)
			if err != nil {
				return err
			}
			if err = llmreadiness.DecodeStrict(data, item.sub); err != nil {
				return err
			}
		}
		report, err := llmreadiness.Score(s, h, sa, sb)
		if err != nil {
			return err
		}
		artifacts = map[string]any{"report.json": report}
	default:
		return fmt.Errorf("unknown command")
	}
	// Refuse existing directories (including symlinks) and never overwrite the
	// source snapshot. Every output is private and diagnostic only.
	if err = os.Mkdir(*out, 0700); err != nil {
		return fmt.Errorf("output directory must be new")
	}
	for name, value := range artifacts {
		if err = write(filepath.Join(*out, name), value); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
