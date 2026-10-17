package model

import (
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Duration time.Duration

func (d Duration) Value() (driver.Value, error) {
	return time.Duration(d).String(), nil
}

func (d *Duration) Scan(value interface{}) error {
	switch v := value.(type) {
	case int64:
		*d = Duration(v)
		return nil
	case []byte:
		return d.Scan(string(v))
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			*d = 0
			return nil
		}
		var total time.Duration
		if idx := strings.Index(v, " day"); idx != -1 {
			days, err := strconv.Atoi(strings.TrimSpace(v[:idx]))
			if err != nil {
				return err
			}
			total += time.Duration(days) * 24 * time.Hour
			rest := strings.TrimSpace(v[idx:])
			rest = strings.TrimPrefix(rest, "days")
			rest = strings.TrimPrefix(rest, "day")
			v = strings.TrimSpace(rest)
			if v == "" {
				*d = Duration(total)
				return nil
			}
		}
		if strings.Contains(v, ":") {
			v = strings.Replace(v, ":", "h", 1)
			v = strings.Replace(v, ":", "m", 1)
			v += "s"
		}
		dur, err := time.ParseDuration(v)
		if err != nil {
			return err
		}
		*d = Duration(total + dur)
		return nil
	default:
		return fmt.Errorf("cannot sql.Scan() PgDuration from: %#v", v)
	}
}
