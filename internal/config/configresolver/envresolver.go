package configresolver

import (
	"fmt"
	"os"
	"reflect"
	"strings"
)

// SecretFileSuffix lets any config value be delivered by file instead of by
// environment variable: `JUNKPURGE_LLM_API_KEY_FILE=/run/secrets/openai_api_key`
// is read in place of `JUNKPURGE_LLM_API_KEY`. Same convention the official
// postgres/mysql images use.
//
// This exists because anything passed as a container environment variable is
// readable by anyone who can reach the docker socket, and by anything that
// scrapes container metadata — `docker inspect` prints it in plaintext. A
// mounted file is not in that metadata. Prefer the `_FILE` form for every
// credential; plain env stays supported for non-secret config and for local
// development.
const SecretFileSuffix = "_FILE"

type envResolver struct {
	baseResolver
	e map[string]string
}

func NewEnv(e map[string]string, options ...Option) Resolver {
	r := &envResolver{e: e}
	r.applyOptions(append([]Option{WithKey("env")}, options...)...)

	return r
}

func (r envResolver) Resolve(path []string, valueType reflect.Type) (interface{}, bool, error) {
	envKey := strings.ToUpper(strings.Join(path, "_"))

	envValue, ok := r.e[envKey]

	// An empty plain value counts as absent for this purpose, so a leftover
	// `FOO=` (compose `${FOO:-}` expands to exactly that) cannot silently
	// shadow the file and disable the feature it configures.
	fromFile := false

	if envValue == "" {
		if filePath, hasFile := r.e[envKey+SecretFileSuffix]; hasFile {
			contents, readErr := os.ReadFile(filePath)
			if readErr != nil {
				return nil, true, fmt.Errorf(
					"error reading env key '%s%s' file '%s': %w",
					envKey,
					SecretFileSuffix,
					filePath,
					readErr,
				)
			}

			// Trailing newline is near-universal in secret files.
			envValue = strings.TrimSpace(string(contents))
			if envValue == "" {
				return nil, true, fmt.Errorf(
					"env key '%s%s' file '%s' is empty",
					envKey,
					SecretFileSuffix,
					filePath,
				)
			}

			ok = true
			fromFile = true
		}
	}

	if !ok {
		return nil, false, nil
	}

	coercedValue, coerceErr := coerceStringValue(envValue, valueType)
	if coerceErr != nil {
		// Deliberately reports neither a file-sourced value nor the underlying
		// error, which echoes it (`strconv.Atoi: parsing "<secret>"`). The key
		// and target type are enough to diagnose a mistyped secret file.
		if fromFile {
			return nil, true, fmt.Errorf(
				"error coercing the value read from env key '%s%s' to type %v"+
					" (value and underlying error suppressed — the file is a secret)",
				envKey,
				SecretFileSuffix,
				valueType,
			)
		}

		return nil, true, fmt.Errorf(
			"error coercing env key '%s' with value '%s' to type %v: %w",
			envKey,
			envValue,
			valueType,
			coerceErr,
		)
	}

	return coercedValue, true, nil
}
