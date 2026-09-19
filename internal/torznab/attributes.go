package torznab

const (
	AttrInfoHash  = "infohash"
	AttrMagnetURL = "magneturl"
	// AttrCategory is the Category ID
	AttrCategory    = "category"
	AttrSize        = "size"
	AttrPublishDate = "publishdate"
	AttrSeeders     = "seeders"
	AttrLeechers    = "leechers"
	AttrPeers       = "peers"
	// AttrSeedsCheckedAt is the RFC3339 timestamp of the authoritative
	// tracker verdict backing the seeders attr — emitted only when a
	// 'tracker' source row exists, so consumers can weigh count freshness.
	// Absent = the counts (if any) are DHT-approximate (honest-unknown).
	AttrSeedsCheckedAt = "seedscheckedat"
	// AttrEnglishAudio is the release's English availability for anime:
	// dub | sub | none (raw). Emitted straight from the persisted
	// torrent_contents.english_audio column (deterministic name signals
	// with LLM backstop — see model.DeriveEnglishAudio); absent = unknown
	// or not anime.
	AttrEnglishAudio = "englishaudio"
	// AttrFiles is the number of files in the torrent
	AttrFiles   = "files"
	AttrYear    = "year"
	AttrSeason  = "season"
	AttrEpisode = "episode"
	// AttrVideo is the video codec
	AttrVideo      = "video"
	AttrResolution = "resolution"
	AttrTeam       = "team"
	AttrImdb       = "imdb"
	AttrTmdb       = "tmdb"
)
