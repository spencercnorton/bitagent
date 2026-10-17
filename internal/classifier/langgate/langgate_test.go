package langgate

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"unicode"
	"unicode/utf8"
)

// checkCase is a case from the Python module's own self-check, with the reason
// the Python module reports for it (the self-check asserts only the class).
type checkCase struct{ name, class, reason string }

// edgeCase is a synthetic input chosen to hit a place where Python's re differs
// from Go's regexp, with the decision the Python module makes for it.
type edgeCase struct {
	name          string
	files         []string
	script, jav   bool
	class, reason string
}

// Cases of the Python self-check run with default flags, then with useScript and useJAV.
var checkCases = []checkCase{
	{"Kiss.Me.Much.2001.KOREAN.1080p.BluRay.H264.AAC-VXT", "non_english", "audio_language_tag:KOREAN"},
	{"Gdy zapada zmrok (2003) [BRRip.XviD.AC3] [Lektor PL] [sav]", "non_english", "audio_language_tag:LEKTOR PL"},
	{"The.Story.Of.The.Last.Chrysanthemum.1939.REMASTERED.JAPANESE", "non_english", "audio_language_tag:JAPANESE"},
	{"Movie.2020.1080p.WEB-DL.DUAL.ENG.RUS", "english", "english_track_tag"},
	{"Parasite.2019.KOREAN.1080p.BluRay.x264-SUBBED", "non_english", "audio_language_tag:KOREAN"},
	{"Dune.2021.1080p.BluRay.x264-RARBG", "undetermined", ""},
	{"Film.2019.MULTi.ENG.1080p", "english", "english_track_tag"},
	{"Amelie.2001.FRENCH.720p.BluRay", "non_english", "audio_language_tag:FRENCH"},
	{"Show.S01E01.iTA.ENG.1080p", "english", "english_track_tag"},
	{"Movie.2019.VOSTFR.1080p", "undetermined", ""},
	{"奥本海默.2023.4K.英语中英双字无水印.mp4", "english", "english_track_tag"},
	{"王牌特工2：黄金圈.2017.1080P.BD.X264.AAC.高清.英语国语音轨.中文字幕", "english", "english_track_tag"},
	{"更好的明天.国语中字.mp4", "non_english", "native_audio_tag"},
	{"Spin City - Season 6 (Rus subs)", "undetermined", ""},
	{"Ordinary.Joe.S01.WEBRip.x265-ION265", "undetermined", ""},
	{"HT-28194 Beretta James", "undetermined", ""},
	{"Portishead - Dummy [ABR-320kbps]", "undetermined", ""},
	{"Mystic river.2003..BDRip.CZ.EN.1080p.mkv", "english", "english_track_tag"},
	{"Lupin (2021) S02 1080p NF Web-DL [EN-FR-TR] DDP5.1 H.264-TURG", "english", "english_track_tag"},
	{"Attila the Hun (2001) 1080p.BluRay.h264-EN+2xBG.audio~bNEW.mkv", "english", "english_track_tag"},
	{"Хоккей.НХЛ.Обзор.720p.30ps.NHL_Network_EN.28.03.2026.mkv", "english", "english_track_tag"},
	{"Partir.en.guerre.2018.FRENCH.720p", "non_english", "audio_language_tag:FRENCH"},
	{"[BitchesAbroad] Anabell - Busty French Babe Gets Cum On Her Face", "undetermined", ""},
	{"The Chinese Question The Gold Rushes", "undetermined", ""},
	{"Normal.People.S01.German.DL.AC3.1080p.WebRip.x265-FuN", "english", "english_track_tag"},
	{"The.Flight.Attendant.S01E03-04.iTALIAN.MULTI.1080p.HMAX.WEB-DL", "english", "english_track_tag"},
	{"X-Files-Season6-HQ-DVDRIP-720x400", "undetermined", ""},
	{"Movie.2020.iTALiAN.1080p.WEB", "non_english", "audio_language_tag:ITALIAN"},
	{"Donut4-Manther-Uses-Tiny-Cock-Teen-To-Cum-HD-001.mp4", "undetermined", ""},
	{"olo@SIS001@MilfsLikeitBig-Ava Addams-紅毯上的啪啪秀", "english", "english_track_tag"},
	// v5: the DL of WEB-DL is not an English track; DUAL needs its listed
	// languages; "sub UKR" names subtitles; lower-case rus/ukr tags and Russian
	// dub studios name non-English audio.
	{"[ Torrent9.info ] The.Affair.S03E03.FRENCH.WEB-DL.XviD-T9.avi", "non_english", "audio_language_tag:FRENCH"},
	{"Movie.2019.1080p.WEB-DL.x264-GRP", "undetermined", ""},
	{"The Battleship Island (2017) 720p BluRay x264 Eng Subs [Dual Audio] [Hindi DD 2.0 - Korean 2.0]", "english", "english_track_tag"},
	{"The.Battleship.Island.2017.Dual.Audio.Hindi.Korean.mkv", "non_english", "dual_audio_without_english"},
	{"Hachi.A.Dogs.Tale.2009.720p.BRRip.Telugu.Dub.Dual-Audio.x264-1XBET.mkv", "english", "english_track_tag"},
	{"PAW.Patrol.Missione.Dinosauri.2026..WebDL.E-AC3.ITA.ENG.SUBS.DD-CREW.mkv", "english", "english_track_tag"},
	{"[Hurtom] Be My Valentine Charlie Brown 1975 WEB-DL [1080p] sub UKR.mkv", "undetermined", ""},
	{"Younger - S05-07 - 1080P H265 AC3 ENG SUB ITA [vrs2]", "english", "english_track_tag"},
	{"Movie.2019.DUAL.1080p.mkv", "english", "english_track_tag"},
	{"Moon.Knight.S01E01.rus.LostFilm.TV.avi", "non_english", "audio_language_tag:RUS"},
	{"Show.S02E05.1080p.WEB-DL.HDRezka.mkv", "non_english", "native_audio_tag"},
	{"Show.S01E01.1080p.AMZN.WEB-DL.DDP5.1.H.264-NTb", "undetermined", ""},
}

var checkScriptCases = []checkCase{
	{"死亡笔记/死亡笔记真人版.Death Note.2017.[BT\u5c0f镇]", "undetermined", "latin_title_alongside_native_script"},
	{"실비아 크리스텔의 개인 교수 Private.Lessons.1981.XviD.AC3-WAF", "undetermined", "latin_title_alongside_native_script"},
	{"僵尸脱衣舞娘", "non_english", "native_script_no_subtitle_marker"},
	{"Скалолазы (2025) WEB-DLRip-AVC_ivanes.mkv", "non_english", "native_script_no_subtitle_marker"},
	{"更好的明天.国语中字.mp4", "non_english", "native_audio_tag"},
	{"SeventhSon(2014)3D-halfOU(Мастер5).mkv", "undetermined", "latin_title_alongside_native_script"},
	{"[2007] Beyonc\u0435 - B'Day (Deluxe Edition).mkv", "undetermined", ""},
	{"上善若水@www.sexinsex.net@【HardX】系列12部高清合集", "undetermined", "latin_title_alongside_native_script"},
	{"歐美九連發20120617", "undetermined", "western_origin_marker"},
}

// Edge cases: Unicode word boundaries and digits, re.I folding of U+0130, U+0131,
// U+017F and U+212A, Python's \s, case-sensitive tags, subtitle markers, reason
// stripping, JAV lookaheads, forum prefixes and title zones. All names are synthetic.
var edgeCases = []edgeCase{
	{"Movie.2020.KOREAN.1080p", nil, false, false, "non_english", "audio_language_tag:KOREAN"},
	{"KOREAN", nil, false, false, "non_english", "audio_language_tag:KOREAN"},
	{".KOREAN.", nil, false, false, "non_english", "audio_language_tag:KOREAN"},
	{"Movie.French.1080p", nil, false, false, "undetermined", ""},
	{"Movie.iTALiAN.1080p", nil, false, false, "non_english", "audio_language_tag:ITALIAN"},
	{"Movie.iTALIAN.1080p", nil, false, false, "undetermined", ""},
	{"Movie.ITA.1080p", nil, false, false, "non_english", "audio_language_tag:ITA"},
	{"Movie.iTA.1080p", nil, false, false, "non_english", "audio_language_tag:ITA"},
	{"Movie[KOREAN]1080p", nil, false, false, "non_english", "audio_language_tag:KOREAN"},
	{"Movie{KOREAN}", nil, false, false, "non_english", "audio_language_tag:KOREAN"},
	{"Movie_KOREAN_x264", nil, false, false, "non_english", "audio_language_tag:KOREAN"},
	{"Movie|KOREAN|x264", nil, false, false, "non_english", "audio_language_tag:KOREAN"},
	{"Movie\u2003KOREAN\u2003x264", nil, false, false, "non_english", "audio_language_tag:KOREAN"},
	{"Movie\x1fKOREAN\x1fx264", nil, false, false, "non_english", "audio_language_tag:KOREAN"},
	{"Movie\u200bKOREAN", nil, false, false, "undetermined", ""},
	{"Movie.KOREAN!.x264", nil, false, false, "undetermined", ""},
	{"Movie.KOREANS.x264", nil, false, false, "undetermined", ""},
	{"Movie.JAPANESE.KOREAN", nil, false, false, "non_english", "audio_language_tag:JAPANESE"},
	{"Movie.KOREAN.ENG", nil, false, false, "english", "english_track_tag"},
	{"Movie.GERMAN.DL.1080p", nil, false, false, "english", "english_track_tag"},
	{"Show.S01.RUS.subs", nil, false, false, "undetermined", ""},
	{"Show.S01.RUS.SUBBED", nil, false, false, "undetermined", ""},
	{"Show.S01.RUS-sub", nil, false, false, "undetermined", ""},
	{"Show.S01.RUS.subtitles", nil, false, false, "undetermined", ""},
	{"Show.S01.RUS.subsX", nil, false, false, "non_english", "audio_language_tag:RUS"},
	{"Show.S01.RUS.subs\u00e9", nil, false, false, "non_english", "audio_language_tag:RUS"},
	{"Show.S01.RUS.subs，1080p", nil, false, false, "undetermined", ""},
	{"Show.S01.RUS.субтитры", nil, false, false, "undetermined", ""},
	{"Show.S01.RUS.SUB.KOREAN", nil, false, false, "undetermined", ""},
	{"Show.S01.RUS.sub.mkv", nil, false, false, "undetermined", ""},
	{"Show S01 RUS subs", nil, false, false, "undetermined", ""},
	{"Show.S01.RUS.SUBTITLE", nil, false, false, "undetermined", ""},
	{"Show.S01.RUS.subs\u0130", nil, false, false, "non_english", "audio_language_tag:RUS"},
	{"Show.S01.RUS.sub\u0131tles", nil, false, false, "non_english", "audio_language_tag:RUS"},
	{"Film.2003.[Lektor PL]", nil, false, false, "non_english", "audio_language_tag:LEKTOR PL"},
	{"Film Lektor.PL", nil, false, false, "non_english", "audio_language_tag:LEKTOR.PL"},
	{"Film.LEKTOR", nil, false, false, "non_english", "audio_language_tag:LEKTOR"},
	{"Film+Dubbing", nil, false, false, "non_english", "audio_language_tag:+DUBBING"},
	{"Film{Lektor}", nil, false, false, "non_english", "audio_language_tag:{LEKTOR"},
	{"Film,Lektor", nil, false, false, "non_english", "audio_language_tag:,LEKTOR"},
	{"Film\u00a0Lektor", nil, false, false, "non_english", "audio_language_tag:\u00a0LEKTOR"},
	{"Lektor", nil, false, false, "non_english", "audio_language_tag:LEKTOR"},
	{"LektorPL", nil, false, false, "non_english", "audio_language_tag:LEKTORPL"},
	{"Lektor PLx", nil, false, false, "non_english", "audio_language_tag:LEKTOR"},
	{"Film.Lektor-POL.x264", nil, false, false, "non_english", "audio_language_tag:POL"},
	{"Film DUBB\u0130NG", nil, false, false, "non_english", "audio_language_tag:DUBB\u0130NG"},
	{"Film dubb\u0131ng PL", nil, false, false, "non_english", "audio_language_tag:DUBBING PL"},
	{"Film.Lektor_", nil, false, false, "non_english", "audio_language_tag:LEKTOR"},
	{"Film Lektor  PL", nil, false, false, "non_english", "audio_language_tag:LEKTOR"},
	{"xLektor", nil, false, false, "undetermined", ""},
	{"Film.Lektors", nil, false, false, "undetermined", ""},
	{"Movie.MULTi.1080p", nil, false, false, "english", "english_track_tag"},
	{"Movie.MULT\u0130.AUD\u0130O.ENG", nil, false, false, "english", "english_track_tag"},
	{"Movie.mult\u0131", nil, false, false, "english", "english_track_tag"},
	{"Movie.ENGL\u0130SH.x264", nil, false, false, "english", "english_track_tag"},
	{"Movie.OR\u0130G\u0130NAL", nil, false, false, "english", "english_track_tag"},
	{"Movie.dubbed", nil, false, false, "english", "english_track_tag"},
	{"Movie.DL", nil, false, false, "undetermined", ""},
	{"Movie.DUAL.AUDIO", nil, false, false, "english", "english_track_tag"},
	{"Movie.Dual-Language", nil, false, false, "english", "english_track_tag"},
	{"Movie.RUS+ENG", nil, false, false, "english", "english_track_tag"},
	{"Movie.\u0130T+ENG", nil, false, false, "english", "english_track_tag"},
	{"Movie.ENG+RUS", nil, false, false, "english", "english_track_tag"},
	{"Movie.ENG&FRA", nil, false, false, "english", "english_track_tag"},
	{"Movie.EN.DUB", nil, false, false, "english", "english_track_tag"},
	{"Movie.ENG.AUDIO", nil, false, false, "english", "english_track_tag"},
	{"Movie.MULTI.AUDIO.ENG", nil, false, false, "english", "english_track_tag"},
	{"NHL_Network_EN.28.03", nil, false, false, "english", "english_track_tag"},
	{"NHL_Network_en.28.03", nil, false, false, "undetermined", ""},
	{"Movie.ENGINEER", nil, false, false, "undetermined", ""},
	{"Show [EN-FR-TR] DDP5.1", nil, false, false, "english", "english_track_tag"},
	{"Show.CZ.EN.1080p", nil, false, false, "english", "english_track_tag"},
	{"Show.EN+2xBG.audio", nil, false, false, "english", "english_track_tag"},
	{"Show.EN+٢xBG.audio", nil, false, false, "english", "english_track_tag"},
	{"Show.EN+𐵀xBG.audio", nil, false, false, "english", "english_track_tag"},
	{"Show.(RU, EN)", nil, false, false, "english", "english_track_tag"},
	{"Movie.KOREAN.Eng.Subs", nil, false, false, "english", "english_track_tag"},
	{"Brazzers.Exxtra.2020", nil, false, false, "english", "english_track_tag"},
	{"Naughty America 2020", nil, false, false, "english", "english_track_tag"},
	{"xBLACKEDx 2020", nil, false, false, "english", "english_track_tag"},
	{"Pr\u0130vate.com 2020", nil, false, false, "english", "english_track_tag"},
	{"Vixen", nil, false, false, "english", "english_track_tag"},
	{"电影.英语.1080p", nil, false, false, "english", "english_track_tag"},
	{"电影.英文字幕.1080p", nil, false, false, "undetermined", ""},
	{"电影.英文字幕.英语", nil, false, false, "english", "english_track_tag"},
	{"电影.原声", nil, false, false, "english", "english_track_tag"},
	{"Фильм.Оригинал.2020", nil, false, false, "english", "english_track_tag"},
	{"Фильм.оригинальный", nil, false, false, "english", "english_track_tag"},
	{"电影.国语.1080p", nil, false, false, "non_english", "native_audio_tag"},
	{"Фильм.Дубляж", nil, false, false, "non_english", "native_audio_tag"},
	{"Film.MVO.2020", nil, false, false, "non_english", "native_audio_tag"},
	{"SomeMVOThing", nil, false, false, "non_english", "native_audio_tag"},
	{"Film.dvo.2020", nil, false, false, "undetermined", ""},
	{"Album.2020", []string{"01.mp3", "02.flac"}, false, false, "undetermined", "not_video"},
	{"Album.2020", []string{"01.mp3", "cover.jpg"}, false, false, "undetermined", "not_video"},
	{"Album.2020", []string{"01.MP3", "movie.mKv"}, false, false, "undetermined", ""},
	{"Album.2020", []string{"01.mp3\n"}, false, false, "undetermined", "not_video"},
	{"Book.2020", []string{"book.epub"}, false, false, "undetermined", "not_video"},
	{"Album.2020", []string{"a.mp3", "a.mkv"}, false, false, "undetermined", ""},
	{"Album.mp3", nil, false, false, "undetermined", "not_video"},
	{"Movie.mkv", nil, false, false, "undetermined", ""},
	{"Movie.m2ts", nil, false, false, "undetermined", ""},
	{"Movie.mp3.KOREAN", nil, false, false, "non_english", "audio_language_tag:KOREAN"},
	{"Album.2020.KOREAN", []string{"01.mp3"}, false, false, "undetermined", "not_video"},
	{"SSIS-001", nil, false, true, "non_english", "jav_code"},
	{"ABP-123 Title", nil, false, true, "non_english", "jav_code"},
	{"xSSIS-001", nil, false, true, "undetermined", ""},
	{"[ABC-123]", nil, false, true, "non_english", "jav_code"},
	{"ABC-123k", nil, false, true, "undetermined", ""},
	{"ABC-123kb", nil, false, true, "undetermined", ""},
	{"ABC-123kbps", nil, false, true, "undetermined", ""},
	{"ABC-123K", nil, false, true, "non_english", "jav_code"},
	{"ABC-123kg", nil, false, true, "non_english", "jav_code"},
	{"ABC-1080", nil, false, true, "undetermined", ""},
	{"ABC-1080p", nil, false, true, "undetermined", ""},
	{"ABC-10801", nil, false, true, "undetermined", ""},
	{"ABC-12345", nil, false, true, "undetermined", ""},
	{"ABC-1234", nil, false, true, "non_english", "jav_code"},
	{"ABC-12", nil, false, true, "undetermined", ""},
	{"ABCDEFG-123", nil, false, true, "undetermined", ""},
	{"ABCDEF-123", nil, false, true, "non_english", "jav_code"},
	{"A-123", nil, false, true, "undetermined", ""},
	{"WEB-123", nil, false, true, "undetermined", ""},
	{"HD-123", nil, false, true, "undetermined", ""},
	{"DVDRIP-1234", nil, false, true, "undetermined", ""},
	{"AC3-123", nil, false, true, "undetermined", ""},
	{"日\u672cABC-123", nil, false, true, "non_english", "jav_code"},
	{"ABC-１２３", nil, false, true, "non_english", "jav_code"},
	{"ABC-123٤", nil, false, true, "non_english", "jav_code"},
	{"ABC-123.mp4", nil, false, true, "non_english", "jav_code"},
	{"ABC-DEF-123", nil, false, true, "non_english", "jav_code"},
	{"abc-123", nil, false, true, "undetermined", ""},
	{"ABC-123 中字", nil, false, true, "undetermined", ""},
	{"ABC-123 字幕", nil, false, true, "undetermined", ""},
	{"ABC-4320", nil, false, true, "undetermined", ""},
	{"ABC-4321", nil, false, true, "non_english", "jav_code"},
	{"ABC-480", nil, false, true, "undetermined", ""},
	{"ABC-480x", nil, false, true, "undetermined", ""},
	{"ABC-123é", nil, false, true, "non_english", "jav_code"},
	{"ABC-12٤", nil, false, true, "non_english", "jav_code"},
	{"ABC-123k\u00e9", nil, false, true, "non_english", "jav_code"},
	{"ABC-123k.mp4", nil, false, true, "undetermined", ""},
	{"ABC-123_", nil, false, true, "non_english", "jav_code"},
	{"ABC-123\n", nil, false, true, "non_english", "jav_code"},
	{"ABC-2160", nil, false, true, "undetermined", ""},
	{"ABC-21601", nil, false, true, "undetermined", ""},
	{"SSIS-001", nil, false, false, "undetermined", ""},
	{"死亡笔记.Death.Note.2017", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"死亡笔记", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"Скалолазы (2025) WEB-DLRip", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"Скалолазы", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文标\u9898WEB-DL.Foo.Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文标题.WEB-DL.Foo.Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文_1080p.Foo.Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 1080p Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文.Foo.Bar.20２０", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中\u6587SeventhSon", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 Don't Go", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 Don't Stop", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 ab cd", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 Foo", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 Foo Bar (2019)", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 Foo [2019] Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 Foo.Bar2019", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 Foo.Bar.1080\u0130.Baz", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 Foo.Bar.1080pX.Baz", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 Foo.x264.Bar.Baz", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 Foo.X264.Bar.Baz", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 Foo.blu-ray.Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 Foo.web.Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 Foo.webx.Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 Foo._web.Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 Foo.\u00e9web.Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 Foo.é web.Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 Fo\u00e9o Ba\u00e9r", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 ’Foo Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"aaaaaaaaaaaaaaaaaaaaaaaa@中文", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"aaaaaaaaaaaaaaaaaaaaaaaaa@中文", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"a@b@c@中文", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"a b@中文", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"@中文", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文@Foo Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"aaaaaaaaaaaaaaaaaaaaaaaa@【HardX】中文", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"a@[HardX]中文", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文【HardX】", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"a@【Ab1】中文", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"a@【Abcd】中文", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"a@【Abcd]中文", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"欧美九连发", nil, true, false, "undetermined", "western_origin_marker"},
	{"好莱坞.Foo.Bar", nil, true, false, "undetermined", "western_origin_marker"},
	{"美剧.Foo.Bar", nil, true, false, "undetermined", "western_origin_marker"},
	{"英剧", nil, true, false, "undetermined", "western_origin_marker"},
	{"中文.中字", nil, true, false, "undetermined", ""},
	{"中文 SUBRIP", nil, true, false, "undetermined", ""},
	{"中文 SUBR\u0130P", nil, true, false, "undetermined", ""},
	{"中文 KOR SUBS", nil, true, false, "undetermined", ""},
	{"中文 kor.sub", nil, true, false, "undetermined", ""},
	{"中文 субтитр", nil, true, false, "undetermined", ""},
	{"中文 СУБТИТР", nil, true, false, "undetermined", ""},
	{"한글 한글자막", nil, true, false, "undetermined", ""},
	{"한글 영화", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"ドラマ", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文", []string{"a.srt"}, true, false, "undetermined", "subtitle_files_present"},
	{"中文", []string{"a.SRT"}, true, false, "undetermined", "subtitle_files_present"},
	{"中文", []string{"a.srt\n"}, true, false, "undetermined", "subtitle_files_present"},
	{"中文", []string{"a.\u017frt"}, true, false, "undetermined", "subtitle_files_present"},
	{"中文", []string{"a.sub", "b.mkv"}, true, false, "undetermined", "subtitle_files_present"},
	{"中文", []string{"a.txt"}, true, false, "undetermined", "not_video"},
	{"中文", []string{"a.srt.bak"}, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 Foo Bar", []string{"a.srt"}, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"бб Foo", []string{"a.idx"}, true, false, "undetermined", ""},
	{"АБ", nil, true, false, "undetermined", ""},
	{"АБВ", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"АБ В", nil, true, false, "undetermined", ""},
	{"Foo АБВ Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"Foo\uff21 Bar", nil, true, false, "undetermined", ""},
	{"中文.DUAL.AUDIO", nil, true, false, "english", "english_track_tag"},
	{"中文.RUS", nil, true, false, "non_english", "audio_language_tag:RUS"},
	{"中文.ENG", nil, true, false, "english", "english_track_tag"},
	{"中文.Foo.Bar.1080p", []string{"a.srt"}, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"😀 Foo Bar", nil, true, false, "undetermined", ""},
	{"☃", nil, true, false, "undetermined", ""},
	{"中文 Foo Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 20２０ Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 19٩٩ Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 20𐵀𐵁 Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 2019 Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 201 Foo Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 480p Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 480i Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 480P Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 576p Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 576i Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 576P Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 720p Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 720i Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 720P Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 1080p Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 1080i Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 1080P Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 2160p Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 2160i Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 2160P Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 4800p Foo Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 1080 Foo Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 1080pp Foo Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 bluray Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 blu-ray Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 web Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 webrip Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 web-dl Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 webdl Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 hdtv Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 dvdrip Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 bdrip Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 brrip Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 hdrip Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 dlrip Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 remux Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 x264 Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 x265 Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 h264 Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 h265 Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 hevc Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 xvid Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 WEB-DL Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 BluRay Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 XviD Foo Bar", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"中文 web2 Foo Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 webrips Foo Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"中文 xx264 Foo Bar", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"AbcAbcAbcAbcAbcAbcAbcAbc@中文", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"AbcAbcAbcAbcAbcAbcAbcAbcA@中文", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"AbcAbc\u4e2d中中中中中中中中中中中中中中中中中@中文", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"AbcAbc\u4e2d中中中中中中中中中中中中中中中中中中@中文", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"Abc@Def@Ghi@中文", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"Abc@Def Ghi@中文", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"Abc@\u3000Def@中文", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"AbcDef@中文@Ghi", nil, true, false, "non_english", "native_script_no_subtitle_marker"},
	{"@@AbcDef\u4e2d文", nil, true, false, "undetermined", "latin_title_alongside_native_script"},
	{"ABC-1080１", nil, false, true, "non_english", "jav_code"},
	{"ABC-1080١", nil, false, true, "non_english", "jav_code"},
	{"ABC-1080 ", nil, false, true, "undetermined", ""},
	{"ABC-576", nil, false, true, "undetermined", ""},
	{"ABC-720", nil, false, true, "undetermined", ""},
	{"ABC-2160", nil, false, true, "undetermined", ""},
	{"ABC-4320", nil, false, true, "undetermined", ""},
	{"ABC-480p", nil, false, true, "undetermined", ""},
	{"ABC-576.mp4", nil, false, true, "undetermined", ""},
	{"ABCDEFG-123", nil, false, true, "undetermined", ""},
	{"ABCDEF-123", nil, false, true, "non_english", "jav_code"},
	{"AB-123", nil, false, true, "non_english", "jav_code"},
	{"ABC-123", nil, false, true, "non_english", "jav_code"},
	{"ABC-12𐵀", nil, false, true, "non_english", "jav_code"},
	{"ABC-𐵀𐵁𐵂", nil, false, true, "non_english", "jav_code"},
	{"Movie.\u0130T&ENG", nil, false, false, "english", "english_track_tag"},
	{"Movie.ENG&\u0130T", nil, false, false, "english", "english_track_tag"},
	{"Movie.\u0131t&ENG", nil, false, false, "english", "english_track_tag"},
	{"Movie.\u212aR&ENG", nil, false, false, "english", "english_track_tag"},
	{"Movie.\u017fp&ENG", nil, false, false, "english", "english_track_tag"},
	{"Movie.ENG&\u017fp", nil, false, false, "english", "english_track_tag"},
	{"Movie.T&ENG", nil, false, false, "undetermined", ""},
	{"Movie.TUVW&ENG", nil, false, false, "undetermined", ""},
	{"Movie.ENG&FR", nil, false, false, "english", "english_track_tag"},
	{"Movie.FR&ENG", nil, false, false, "english", "english_track_tag"},
	{"Movie.ENG&FRAN", nil, false, false, "undetermined", ""},
	{"Movie.ENG AND DE", nil, false, false, "english", "english_track_tag"},
	{"Movie.ENGANDDE", nil, false, false, "english", "english_track_tag"},
	{"Movie.ENG-AND-D", nil, false, false, "english", "english_track_tag"},
	{"Movie.KOREAN.国语", nil, false, false, "non_english", "audio_language_tag:KOREAN"},
	{"国语.Movie.FRENCH", nil, false, false, "non_english", "audio_language_tag:FRENCH"},
	{"Movie.ENG.国语", nil, false, false, "english", "english_track_tag"},
	{"ABC-123 国语", nil, false, true, "non_english", "native_audio_tag"},
	{"国语 Foo Bar", nil, true, false, "non_english", "native_audio_tag"},
	{"ABC-123 Дубляж", nil, true, true, "non_english", "native_audio_tag"},
	{"ABC-123 KOREAN", nil, true, true, "non_english", "audio_language_tag:KOREAN"},
	{"Movie.英语", nil, false, false, "english", "english_track_tag"},
	{"Movie.英语字幕", nil, false, false, "undetermined", ""},
	{"Movie.英語", nil, false, false, "english", "english_track_tag"},
	{"Movie.英語字幕", nil, false, false, "undetermined", ""},
	{"Movie.英文", nil, false, false, "english", "english_track_tag"},
	{"Movie.英文字幕", nil, false, false, "undetermined", ""},
	{"Movie.英配", nil, false, false, "english", "english_track_tag"},
	{"Movie.英配字幕", nil, false, false, "undetermined", ""},
	{"Movie.原声", nil, false, false, "english", "english_track_tag"},
	{"Movie.原声字幕", nil, false, false, "undetermined", ""},
	{"Movie.原聲", nil, false, false, "english", "english_track_tag"},
	{"Movie.原聲字幕", nil, false, false, "undetermined", ""},
	{"Movie.国英", nil, false, false, "english", "english_track_tag"},
	{"Movie.国英字幕", nil, false, false, "undetermined", ""},
	{"Movie.國英", nil, false, false, "english", "english_track_tag"},
	{"Movie.國英字幕", nil, false, false, "undetermined", ""},
	{"Movie.英粤", nil, false, false, "english", "english_track_tag"},
	{"Movie.英粤字幕", nil, false, false, "undetermined", ""},
	{"Movie.英粵", nil, false, false, "english", "english_track_tag"},
	{"Movie.英粵字幕", nil, false, false, "undetermined", ""},
	{"Movie.英音", nil, false, false, "english", "english_track_tag"},
	{"Movie.英音字幕", nil, false, false, "undetermined", ""},
	{"Movie.Оригинал", nil, false, false, "english", "english_track_tag"},
	{"Movie.Оригинал字幕", nil, false, false, "undetermined", ""},
	{"Movie.оригинал", nil, false, false, "english", "english_track_tag"},
	{"Movie.оригинал字幕", nil, false, false, "undetermined", ""},
	{"电影.英文字幕.英文", nil, false, false, "english", "english_track_tag"},
	{"电影.英文字.英文字", nil, false, false, "undetermined", ""},
	{"Movie.国语", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.國語", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.粤语", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.粵語", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.日语", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.日語", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.韩语", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.韓語", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.普通话", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.普通話", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.俄语", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.Дубляж", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.дубляж", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.Многоголос", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.многоголос", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.Одноголос", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.одноголос", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.MVO", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.DVO", nil, false, false, "non_english", "native_audio_tag"},
	{"Movie.AVO", nil, false, false, "non_english", "native_audio_tag"},
	{"欧美 Foo Bar", nil, true, false, "undetermined", "western_origin_marker"},
	{"歐美 Foo Bar", nil, true, false, "undetermined", "western_origin_marker"},
	{"好莱坞 Foo Bar", nil, true, false, "undetermined", "western_origin_marker"},
	{"好萊塢 Foo Bar", nil, true, false, "undetermined", "western_origin_marker"},
	{"美剧 Foo Bar", nil, true, false, "undetermined", "western_origin_marker"},
	{"美劇 Foo Bar", nil, true, false, "undetermined", "western_origin_marker"},
	{"英剧 Foo Bar", nil, true, false, "undetermined", "western_origin_marker"},
	{"英劇 Foo Bar", nil, true, false, "undetermined", "western_origin_marker"},
	{"ABC-123 中字", nil, false, true, "undetermined", ""},
	{"ABC-123 中文字幕", nil, false, true, "undetermined", ""},
	{"ABC-123 中英字幕", nil, false, true, "undetermined", ""},
	{"ABC-123 中英双字", nil, false, true, "undetermined", ""},
	{"ABC-123 中英雙字", nil, false, true, "undetermined", ""},
	{"ABC-123 简中", nil, false, true, "undetermined", ""},
	{"ABC-123 繁中", nil, false, true, "undetermined", ""},
	{"ABC-123 字幕", nil, false, true, "undetermined", ""},
	{"ABC-123 KOR SUBS", nil, false, true, "undetermined", ""},
	{"ABC-123 한글자막", nil, false, true, "undetermined", ""},
	{"ABC-123 субтитр", nil, false, true, "undetermined", ""},
	{"ABC-123 SUBRIP", nil, false, true, "undetermined", ""},
	{"xx brazzers yy", nil, false, false, "english", "english_track_tag"},
	{"BRAZZERS", nil, false, false, "english", "english_track_tag"},
	{"xx naughty america yy", nil, false, false, "english", "english_track_tag"},
	{"NAUGHTYAMERICA", nil, false, false, "english", "english_track_tag"},
	{"xx reality kings yy", nil, false, false, "english", "english_track_tag"},
	{"REALITYKINGS", nil, false, false, "english", "english_track_tag"},
	{"xx bang bros yy", nil, false, false, "english", "english_track_tag"},
	{"BANGBROS", nil, false, false, "english", "english_track_tag"},
	{"xx darkx yy", nil, false, false, "english", "english_track_tag"},
	{"DARKX", nil, false, false, "english", "english_track_tag"},
	{"xx blacked yy", nil, false, false, "english", "english_track_tag"},
	{"BLACKED", nil, false, false, "english", "english_track_tag"},
	{"xx tushy yy", nil, false, false, "english", "english_track_tag"},
	{"TUSHY", nil, false, false, "english", "english_track_tag"},
	{"xx vixen yy", nil, false, false, "english", "english_track_tag"},
	{"VIXEN", nil, false, false, "english", "english_track_tag"},
	{"xx deeper yy", nil, false, false, "english", "english_track_tag"},
	{"DEEPER", nil, false, false, "english", "english_track_tag"},
	{"xx public agent yy", nil, false, false, "english", "english_track_tag"},
	{"PUBLICAGENT", nil, false, false, "english", "english_track_tag"},
	{"xx fake taxi yy", nil, false, false, "english", "english_track_tag"},
	{"FAKETAXI", nil, false, false, "english", "english_track_tag"},
	{"xx fake hub yy", nil, false, false, "english", "english_track_tag"},
	{"FAKEHUB", nil, false, false, "english", "english_track_tag"},
	{"xx milfs like it big yy", nil, false, false, "english", "english_track_tag"},
	{"MILFSLIKEITBIG", nil, false, false, "english", "english_track_tag"},
	{"xx team skeet yy", nil, false, false, "english", "english_track_tag"},
	{"TEAMSKEET", nil, false, false, "english", "english_track_tag"},
	{"xx mofos yy", nil, false, false, "english", "english_track_tag"},
	{"MOFOS", nil, false, false, "english", "english_track_tag"},
	{"xx digital playground yy", nil, false, false, "english", "english_track_tag"},
	{"DIGITALPLAYGROUND", nil, false, false, "english", "english_track_tag"},
	{"xx evil angel yy", nil, false, false, "english", "english_track_tag"},
	{"EVILANGEL", nil, false, false, "english", "english_track_tag"},
	{"xx jules jordan yy", nil, false, false, "english", "english_track_tag"},
	{"JULESJORDAN", nil, false, false, "english", "english_track_tag"},
	{"xx porn pros yy", nil, false, false, "english", "english_track_tag"},
	{"PORNPROS", nil, false, false, "english", "english_track_tag"},
	{"xx nubiles yy", nil, false, false, "english", "english_track_tag"},
	{"NUBILES", nil, false, false, "english", "english_track_tag"},
	{"xx x-art yy", nil, false, false, "english", "english_track_tag"},
	{"X-ART", nil, false, false, "english", "english_track_tag"},
	{"xx passion-hd yy", nil, false, false, "english", "english_track_tag"},
	{"PASSION-HD", nil, false, false, "english", "english_track_tag"},
	{"xx tiny4k yy", nil, false, false, "english", "english_track_tag"},
	{"TINY4K", nil, false, false, "english", "english_track_tag"},
	{"xx cum4k yy", nil, false, false, "english", "english_track_tag"},
	{"CUM4K", nil, false, false, "english", "english_track_tag"},
	{"xx exotic4k yy", nil, false, false, "english", "english_track_tag"},
	{"EXOTIC4K", nil, false, false, "english", "english_track_tag"},
	{"xx lubed yy", nil, false, false, "english", "english_track_tag"},
	{"LUBED", nil, false, false, "english", "english_track_tag"},
	{"xx holed yy", nil, false, false, "english", "english_track_tag"},
	{"HOLED", nil, false, false, "english", "english_track_tag"},
	{"xx spy fam yy", nil, false, false, "english", "english_track_tag"},
	{"SPYFAM", nil, false, false, "english", "english_track_tag"},
	{"xx family strokes yy", nil, false, false, "english", "english_track_tag"},
	{"FAMILYSTROKES", nil, false, false, "english", "english_track_tag"},
	{"xx bratty sis yy", nil, false, false, "english", "english_track_tag"},
	{"BRATTYSIS", nil, false, false, "english", "english_track_tag"},
	{"xx my friends hot mom yy", nil, false, false, "english", "english_track_tag"},
	{"MYFRIENDSHOTMOM", nil, false, false, "english", "english_track_tag"},
	{"xx dirty wives club yy", nil, false, false, "english", "english_track_tag"},
	{"DIRTYWIVESCLUB", nil, false, false, "english", "english_track_tag"},
	{"xx big wet asses yy", nil, false, false, "english", "english_track_tag"},
	{"BIGWETASSES", nil, false, false, "english", "english_track_tag"},
	{"xx big tits at work yy", nil, false, false, "english", "english_track_tag"},
	{"BIGTITSATWORK", nil, false, false, "english", "english_track_tag"},
	{"xx teens like it big yy", nil, false, false, "english", "english_track_tag"},
	{"TEENSLIKEITBIG", nil, false, false, "english", "english_track_tag"},
	{"xx hussie pass yy", nil, false, false, "english", "english_track_tag"},
	{"HUSSIEPASS", nil, false, false, "english", "english_track_tag"},
	{"xx private.com yy", nil, false, false, "english", "english_track_tag"},
	{"PRIVATE.COM", nil, false, false, "english", "english_track_tag"},
	{"xx 21 sextury yy", nil, false, false, "english", "english_track_tag"},
	{"21SEXTURY", nil, false, false, "english", "english_track_tag"},
	{"xx dorcel yy", nil, false, false, "english", "english_track_tag"},
	{"DORCEL", nil, false, false, "english", "english_track_tag"},
	{"xx kink.com yy", nil, false, false, "english", "english_track_tag"},
	{"KINK.COM", nil, false, false, "english", "english_track_tag"},
	{"naughty  america", nil, false, false, "undetermined", ""},
	{"kink com", nil, false, false, "undetermined", ""},
	{"private,com", nil, false, false, "undetermined", ""},
	{"ABC-123 中文", nil, true, true, "non_english", "jav_code"},
	{"中文 ABC-123", nil, true, true, "non_english", "jav_code"},
	{"中文 ABC-123 中字", nil, true, true, "undetermined", ""},
}

func TestCheckCases(t *testing.T) {
	for _, c := range checkCases {
		if class, reason := Evidence(c.name, nil, false, false); class != c.class || reason != c.reason {
			t.Errorf("Evidence(%q) = %q, %q; want %q, %q", c.name, class, reason, c.class, c.reason)
		}
	}
	for _, c := range checkScriptCases {
		if class, reason := Evidence(c.name, nil, true, true); class != c.class || reason != c.reason {
			t.Errorf("Evidence(%q, script, jav) = %q, %q; want %q, %q", c.name, class, reason, c.class, c.reason)
		}
	}
}

func TestEdgeCases(t *testing.T) {
	for _, c := range edgeCases {
		if class, reason := Evidence(c.name, c.files, c.script, c.jav); class != c.class || reason != c.reason {
			t.Errorf("Evidence(%q, %q, script=%v, jav=%v) = %q, %q; want %q, %q", c.name, c.files, c.script, c.jav, class, reason, c.class, c.reason)
		}
	}
}

// TestPythonUnicodeClasses checks isWord, isDigit and isSpace against Python's \w,
// \d and \s for every code point, using ranges read from CPython (see the data file).
func TestPythonUnicodeClasses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "python_unicode_classes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Python, Unicode string
		W, D, S         [][2]rune
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	t.Logf("reference: Python %s, Unicode %s; Go Unicode %s", data.Python, data.Unicode, unicode.Version)
	digitRE := regexp.MustCompile("^" + digitClass() + "$")
	for _, class := range []struct {
		name   string
		ranges [][2]rune
		fn     func(rune) bool
	}{{`\w`, data.W, isWord}, {`\d`, data.D, isDigit}, {`\s`, data.S, isSpace}} {
		want := make([]bool, unicode.MaxRune+1)
		for _, r := range class.ranges {
			for c := r[0]; c <= r[1]; c++ {
				want[c] = true
			}
		}
		bad := 0
		for c := rune(0); c <= unicode.MaxRune; c++ {
			if class.name == `\d` && utf8.ValidRune(c) && digitRE.MatchString(string(c)) != want[c] {
				if bad++; bad <= 5 {
					t.Errorf("digitClass: U+%04X is %v in the Go class, %v in Python", c, !want[c], want[c])
				}
			}
			if utf8.ValidRune(c) && class.fn(c) != want[c] {
				if bad++; bad <= 5 {
					t.Errorf("%s: U+%04X is %v in Go, %v in Python", class.name, c, !want[c], want[c])
				}
			}
		}
		if bad > 0 {
			t.Errorf("%s: %d code points differ", class.name, bad)
		}
	}
}

// TestCanon pins the code points that Python's re.I treats as ASCII letters.
func TestCanon(t *testing.T) {
	for r, want := range map[rune]rune{'A': 'a', 'z': 'z', '3': '3', 0x130: 'i', 0x131: 'i', 0x17F: 's', 0x212A: 'k', 'é': 'é'} {
		if got := canon(r); got != want {
			t.Errorf("canon(%U) = %U, want %U", r, got, want)
		}
	}
}

// oracleRow is one line of the Python decisions: the inputs and the (class, reason)
// pair for each combination of the useJAV and useScript flags.
type oracleRow struct {
	Name  string    `json:"name"`
	Files []string  `json:"files"`
	J0S0  [2]string `json:"jav0_script0"`
	J0S1  [2]string `json:"jav0_script1"`
	J1S0  [2]string `json:"jav1_script0"`
	J1S1  [2]string `json:"jav1_script1"`
}

var oracleCombos = [4]struct {
	label       string
	script, jav bool
}{{"jav0_script0", false, false}, {"jav0_script1", true, false}, {"jav1_script0", false, true}, {"jav1_script1", true, true}}

// oraclePath is the Python decisions file, from $LANGGATE_ORACLE. The file
// holds real torrent names, so it is generated locally from the Python module
// and never committed; the parity test skips when it is absent.
func oraclePath() string {
	return os.Getenv("LANGGATE_ORACLE")
}

// readOracle calls fn for every row, or skips the test when the file is absent.
func readOracle(tb testing.TB, fn func(oracleRow)) {
	tb.Helper()
	path := oraclePath()
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) || path == "" {
		tb.Skipf("parity oracle not present (set LANGGATE_ORACLE): %s", path)
	}
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<20))
	for {
		var row oracleRow
		if err := dec.Decode(&row); errors.Is(err, io.EOF) {
			return
		} else if err != nil {
			tb.Fatal(err)
		}
		fn(row)
	}
}

// TestParityOracle replays the Python decisions on real torrent names and requires
// the same class and the same reason for all four flag combinations.
func TestParityOracle(t *testing.T) {
	rows := 0
	var mismatches [4]int
	readOracle(t, func(row oracleRow) {
		rows++
		want := [4][2]string{row.J0S0, row.J0S1, row.J1S0, row.J1S1}
		for i, c := range oracleCombos {
			class, reason := Evidence(row.Name, row.Files, c.script, c.jav)
			if class == want[i][0] && reason == want[i][1] {
				continue
			}
			if mismatches[i]++; mismatches[i] <= 10 {
				t.Errorf("%s %q files=%q: got %q, %q; want %q, %q", c.label, row.Name, row.Files, class, reason, want[i][0], want[i][1])
			}
		}
	})
	if rows == 0 {
		t.Fatal("oracle holds no rows")
	}
	t.Logf("%d rows; mismatches per combination: %s=%d %s=%d %s=%d %s=%d", rows,
		oracleCombos[0].label, mismatches[0], oracleCombos[1].label, mismatches[1],
		oracleCombos[2].label, mismatches[2], oracleCombos[3].label, mismatches[3])
}

var sink string

// BenchmarkEvidence measures one Evidence call over the oracle's names.
func BenchmarkEvidence(b *testing.B) {
	var rows []oracleRow
	readOracle(b, func(row oracleRow) { rows = append(rows, row) })
	for _, c := range [2]struct {
		label       string
		script, jav bool
	}{{"default_flags", false, false}, {"script_and_jav", true, true}} {
		b.Run(c.label, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				row := &rows[i%len(rows)]
				_, sink = Evidence(row.Name, row.Files, c.script, c.jav)
			}
		})
	}
}
