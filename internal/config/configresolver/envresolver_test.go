package configresolver

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var stringType = reflect.TypeOf("")

func writeSecret(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	return path
}

func TestEnvResolverPlainValueWins(t *testing.T) {
	r := NewEnv(map[string]string{
		"JUNKPURGE_LLM_API_KEY":      "from-env",
		"JUNKPURGE_LLM_API_KEY_FILE": writeSecret(t, "from-file"),
	})

	v, ok, err := r.Resolve([]string{"junkpurge", "llm_api_key"}, stringType)
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}

	if v != "from-env" {
		t.Fatalf("plain env value should win, got %q", v)
	}
}

func TestEnvResolverReadsFileWhenKeyAbsent(t *testing.T) {
	// Trailing newline is what `echo secret > file` produces.
	r := NewEnv(map[string]string{
		"JUNKPURGE_LLM_API_KEY_FILE": writeSecret(t, "sk-proj-fake\n"),
	})

	v, ok, err := r.Resolve([]string{"junkpurge", "llm_api_key"}, stringType)
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}

	if v != "sk-proj-fake" {
		t.Fatalf("want trimmed file contents, got %q", v)
	}
}

// Regression guard for the actual migration footgun: compose `${FOO:-}`
// expands to an empty string, which must not shadow the file.
func TestEnvResolverEmptyPlainValueDoesNotShadowFile(t *testing.T) {
	r := NewEnv(map[string]string{
		"JUNKPURGE_LLM_API_KEY":      "",
		"JUNKPURGE_LLM_API_KEY_FILE": writeSecret(t, "sk-proj-fake"),
	})

	v, ok, err := r.Resolve([]string{"junkpurge", "llm_api_key"}, stringType)
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}

	if v != "sk-proj-fake" {
		t.Fatalf("file should win over empty env, got %q", v)
	}
}

func TestEnvResolverEmptyPlainValueWithoutFileStillResolves(t *testing.T) {
	r := NewEnv(map[string]string{"JUNKPURGE_LLM_API_KEY": ""})

	v, ok, err := r.Resolve([]string{"junkpurge", "llm_api_key"}, stringType)
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}

	if v != "" {
		t.Fatalf("want empty string, got %q", v)
	}
}

func TestEnvResolverAbsentKeyUnresolved(t *testing.T) {
	r := NewEnv(map[string]string{})

	if _, ok, err := r.Resolve([]string{"junkpurge", "llm_api_key"}, stringType); ok || err != nil {
		t.Fatalf("want unresolved, got ok=%v err=%v", ok, err)
	}
}

// Fail loudly rather than starting with a silently dead LLM tier.
func TestEnvResolverMissingAndEmptyFilesError(t *testing.T) {
	for name, path := range map[string]string{
		"missing": filepath.Join(t.TempDir(), "nope"),
		"empty":   writeSecret(t, "  \n"),
	} {
		t.Run(name, func(t *testing.T) {
			r := NewEnv(map[string]string{"JUNKPURGE_LLM_API_KEY_FILE": path})

			_, ok, err := r.Resolve([]string{"junkpurge", "llm_api_key"}, stringType)
			if err == nil {
				t.Fatal("want error")
			}

			if !ok {
				t.Fatal("want ok=true so resolution stops rather than falling through to the default")
			}
		})
	}
}

func TestEnvResolverCoerceErrorDoesNotEchoFileValue(t *testing.T) {
	secret := "sk-proj-not-a-number"
	r := NewEnv(map[string]string{"JUNKPURGE_LLM_TIMEOUT_FILE": writeSecret(t, secret)})

	_, _, err := r.Resolve([]string{"junkpurge", "llm_timeout"}, reflect.TypeOf(0))
	if err == nil {
		t.Fatal("want coercion error")
	}

	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the secret: %v", err)
	}
}
