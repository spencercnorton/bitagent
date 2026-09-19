package contentfilter

import (
	"strings"
	"testing"

	"bytes"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// TestTokensMetricReachesTheScrape is the by-effect check. A metric that
// compiles, is registered, and is never emitted looks identical to one that
// does not exist — which is exactly how item 3 came to claim that
// bitagent_classifier_llm_match_call_errors_total was unimplemented when it is
// fully wired (a CounterVec emits nothing until a label pair is observed).
func TestTokensMetricReachesTheScrape(t *testing.T) {
	m := NewMetrics()
	reg := prometheus.NewRegistry()
	for _, c := range m.Collectors() {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	m.LLMCallbacks().OnLLMTokens("amazon/nova-micro-v1", TokenUsage{
		PromptTokens: 137, CompletionTokens: 21, CachedInputTokens: 40, ReasoningTokens: 3,
	})
	m.LLMCallbacks().OnLLMUsageMissing("openai/gpt-5.4-nano")

	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, f := range fams {
		if err := enc.Encode(f); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	out := buf.String()
	for _, want := range []string{
		`bitagent_contentfilter_llm_tokens_total{kind="input",model="amazon/nova-micro-v1"} 137`,
		`bitagent_contentfilter_llm_tokens_total{kind="output",model="amazon/nova-micro-v1"} 21`,
		`bitagent_contentfilter_llm_tokens_total{kind="cached_input",model="amazon/nova-micro-v1"} 40`,
		`bitagent_contentfilter_llm_tokens_total{kind="reasoning",model="amazon/nova-micro-v1"} 3`,
		`bitagent_contentfilter_llm_usage_missing_total{model="openai/gpt-5.4-nano"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape is missing:\n  %s", want)
		}
	}
	t.Logf("scraped:\n%s", grepLines(out, "llm_tokens_total"))
}

func grepLines(s, sub string) string {
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			b.WriteString("  " + l + "\n")
		}
	}
	return b.String()
}
