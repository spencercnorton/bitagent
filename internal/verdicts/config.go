package verdicts

// Config controls the phase-B ledger READERS (design §4). The ledger
// writers (junkpurge/operator dual-writes from phase A) are always on and
// are not governed here — the ledger must never break the mechanism it
// observes.
type Config struct {
	// ReadersEnabled promotes the phase-B readers (torznab exclusion,
	// crawler pre-fetch check) from shadow to live. While false (the
	// default) the readers still CONSULT the ledger and emit
	// bitagent_verdicts_reader_total shadow metrics, but serving and
	// crawling behavior is unchanged — the design gate: shadow metrics
	// reviewed first, then flag flip.
	ReadersEnabled bool `yaml:"readers_enabled"`
}

func NewDefaultConfig() Config {
	return Config{
		ReadersEnabled: false,
	}
}
