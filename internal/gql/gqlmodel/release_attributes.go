package gqlmodel

type ReleaseAttributesFilterInput struct {
	HDRFormat     *string
	AudioFormat   *string
	AudioFeature  *string
	Revision      *string
	Encoder       *string
	AudioChannels *string
}

func (f ReleaseAttributesFilterInput) values() map[string]*string {
	return map[string]*string{"hdrFormats": f.HDRFormat, "audioFormats": f.AudioFormat,
		"audioFeatures": f.AudioFeature, "revisions": f.Revision,
		"encoder": f.Encoder, "audioChannels": f.AudioChannels}
}
