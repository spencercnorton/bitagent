package classification

// ApplicationPreservation identifies facts already committed by a narrow
// source-bound adapter. Persistence must recheck this token and its target
// snapshot under its own transaction before writing a normal refresh.
type ApplicationPreservation struct {
	TaskKey, SourceDigest, PolicyDigest, SnapshotDigest []byte
	Kind                                                string
	Tags                                                []string
}
