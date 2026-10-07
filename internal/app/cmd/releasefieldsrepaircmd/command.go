// Package releasefieldsrepaircmd exposes frozen, journalled field-only repairs.
package releasefieldsrepaircmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/releasefields"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	Pool       lazy.Lazy[*pgxpool.Pool]
	Classifier classifier.Config
}
type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	flags := []cli.Flag{&cli.StringFlag{Name: "plan", Required: true, Usage: "frozen plan JSON path"}, &cli.BoolFlag{Name: "write", Usage: "commit journalled changes; default only reports proposals"}}
	return Result{Command: &cli.Command{Name: "release-fields-repair", Usage: "Freeze, inspect, apply or restore a bounded filename-claim cohort", Subcommands: []*cli.Command{
		{Name: "freeze", Flags: []cli.Flag{&cli.StringFlag{Name: "cohort", Required: true, Usage: "JSON array of 1..1000 info hashes"}, &cli.StringFlag{Name: "output", Required: true, Usage: "new plan JSON path"}}, Action: func(ctx *cli.Context) error {
			raw, err := readBounded(ctx.String("cohort"))
			if err != nil {
				return err
			}
			var hashes []protocol.ID
			if err = decode(raw, &hashes); err != nil {
				return err
			}
			pool, err := p.Pool.Get()
			if err != nil {
				return err
			}
			plan, err := releasefields.Freeze(ctx.Context, pool, hashes, p.Classifier.ParseNoiseV2)
			if err != nil {
				return err
			}
			return save(ctx.String("output"), plan)
		}},
		{Name: "apply", Flags: flags, Action: func(ctx *cli.Context) error { return p.run(ctx, false) }},
		{Name: "rollback", Flags: flags, Action: func(ctx *cli.Context) error { return p.run(ctx, true) }},
	}}}, nil
}
func (p Params) run(ctx *cli.Context, rollback bool) error {
	raw, err := readBounded(ctx.String("plan"))
	if err != nil {
		return err
	}
	var plan releasefields.Plan
	if err = decode(raw, &plan); err != nil {
		return err
	}
	pool, err := p.Pool.Get()
	if err != nil {
		return err
	}
	var outcomes []releasefields.Outcome
	if rollback {
		outcomes, err = releasefields.Rollback(ctx.Context, pool, plan, ctx.Bool("write"))
	} else {
		outcomes, err = releasefields.Apply(ctx.Context, pool, plan, ctx.Bool("write"))
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(ctx.App.Writer).Encode(outcomes)
}
func readBounded(path string) ([]byte, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if stat.Size() > 4<<20 {
		return nil, fmt.Errorf("repair input exceeds 4 MiB")
	}
	return os.ReadFile(path)
}
func decode(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing repair JSON")
	}
	return nil
}
func save(path string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if _, err = os.Stat(path); err == nil {
		return fmt.Errorf("output already exists")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".release-plan-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Link(tmp.Name(), path)
}
