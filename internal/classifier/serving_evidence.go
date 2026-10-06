package classifier

// CoreAdultServingEvidence returns the shipped, precision-vetted strong tier
// and media extensions. Consumer policy never widens it using private keyword
// overrides, weak single-word matches or matcher-only abstention patterns.
func CoreAdultServingEvidence() ([]string, []string, error) {
	src, err := (yamlSourceProvider{rawSourceProvider: coreSourceProvider{}}).source()
	if err != nil {
		return nil, nil, err
	}
	strong := append([]string(nil), src.Keywords["xxx_strong"]...)
	media := append([]string(nil), src.Extensions["video"]...)
	media = append(media, src.Extensions["image"]...)
	return strong, media, nil
}
