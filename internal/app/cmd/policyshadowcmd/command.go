// Package policyshadowcmd evaluates a bounded public catalogue cohort locally.
package policyshadowcmd

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/cataloguepolicy"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	Pool lazy.Lazy[*pgxpool.Pool]
}
type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) Result {
	return Result{Command: &cli.Command{Name: "catalogue-policy-shadow", Usage: "Evaluate English and availability evidence locally, without changing catalogue eligibility",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "limit", Value: 128, Usage: "public hashes to inspect (1..1024); each page is at most 128"},
			&cli.StringFlag{Name: "after-hash", Usage: "resume after this 40-character hexadecimal hash"},
			&cli.BoolFlag{Name: "write", Usage: "persist bounded shadow receipts; default reads only"},
			&cli.BoolFlag{Name: "anime-english-subs", Usage: "evaluate the draft anime English-subtitle allowance explicitly"},
		}, Action: func(c *cli.Context) error {
			limit := c.Int("limit")
			if limit < 1 || limit > 1024 {
				return fmt.Errorf("limit must be 1..1024")
			}
			after, err := parseCursor(c.String("after-hash"))
			if err != nil {
				return err
			}
			pool, err := p.Pool.Get()
			if err != nil {
				return err
			}
			store := cataloguepolicy.NewStore(pool)
			st := summary{Mode: "dry_run", English: map[string]int{}, Availability: map[string]int{}}
			if c.Bool("write") {
				st.Mode = "shadow_write"
			}
			for st.Scanned < limit {
				page := min(128, limit-st.Scanned)
				hashes, e := store.Candidates(c.Context, after, page)
				if e != nil {
					return e
				}
				if len(hashes) == 0 {
					break
				}
				for _, h := range hashes {
					receipt, e := store.Evaluate(c.Context, h, c.Bool("write"), c.Bool("anime-english-subs"), time.Now().UTC(), cataloguepolicy.DefaultAvailabilityConfig())
					if e != nil {
						return e
					}
					st.Scanned++
					st.English[receipt.English.State]++
					st.Availability[receipt.Availability.State]++
					after = h
				}
				if len(hashes) < page {
					break
				}
			}
			st.NextAfterHash = hex.EncodeToString(after)
			return json.NewEncoder(c.App.Writer).Encode(st)
		}}}
}

type summary struct {
	Mode          string         `json:"mode"`
	Scanned       int            `json:"scanned"`
	English       map[string]int `json:"english"`
	Availability  map[string]int `json:"availability"`
	NextAfterHash string         `json:"next_after_hash"`
}

func parseCursor(s string) ([]byte, error) {
	if s == "" {
		return []byte{}, nil
	}
	if len(s) != 40 {
		return nil, fmt.Errorf("after-hash must have 40 hexadecimal characters")
	}
	h, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("after-hash: %w", err)
	}
	return h, nil
}
