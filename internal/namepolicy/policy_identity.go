package namepolicy

// BindWorkPolicy leaves disabled deployments byte-for-byte compatible, while
// enabled queues/application receipts bind the actual decision definition and
// configured hash set. The internal credential is excluded by Fingerprint.
func BindWorkPolicy(p *Policy, policy any) any {
	if !p.Enabled() {
		return policy
	}
	return []any{policy, p.Fingerprint()}
}
