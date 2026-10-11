package ytx

import "testing"

// testAudioFormat builds a minimal audio Format for selection tests.
func testAudioFormat(itag, bitrate int, mime string, url string, sigCipher string) Format {
	return Format{
		Itag:            itag,
		Bitrate:         bitrate,
		MimeType:        mime,
		URL:             url,
		SignatureCipher: sigCipher,
	}
}

func testMusicSelector() *Extractor {
	return &Extractor{config: GetClientConfig(ModeMusic), audioFormat: AudioFormatWebm}
}

func TestFindBestAudioMusic251Over140Signed(t *testing.T) {
	e := testMusicSelector()
	formats := []Format{
		testAudioFormat(140, 133465, `audio/mp4; codecs="mp4a.40.2"`, "", "url=http%3A%2F%2Fx%2F140.mp4&s=abc&sp=sig"),
		testAudioFormat(251, 157841, `audio/webm; codecs="opus"`, "", "url=http%3A%2F%2Fx%2F251.webm&s=def&sp=sig"),
	}
	got := e.findBestAudioStream(formats)
	if got == nil || got.Itag != 251 {
		t.Fatalf("music default (webm) must prefer 251 over 140 for signed formats, got %#v", got)
	}
}

func TestFindBestAudioMusic251Over140Direct(t *testing.T) {
	e := testMusicSelector()
	formats := []Format{
		testAudioFormat(140, 133465, `audio/mp4; codecs="mp4a.40.2"`, "https://x/140.mp4", ""),
		testAudioFormat(251, 157841, `audio/webm; codecs="opus"`, "https://x/251.webm", ""),
	}
	got := e.findBestAudioStream(formats)
	if got == nil || got.Itag != 251 {
		t.Fatalf("music default (webm) must prefer 251 over 140 for direct formats, got %#v", got)
	}
}

func TestFindBestAudioMusic141CipherBeats140Direct(t *testing.T) {
	e := testMusicSelector()
	formats := []Format{
		testAudioFormat(140, 133465, `audio/mp4; codecs="mp4a.40.2"`, "https://x/140.mp4", ""),
		testAudioFormat(141, 256000, `audio/mp4; codecs="mp4a.40.2"`, "", "url=http%3A%2F%2Fx%2F141.mp4&s=abc&sp=sig"),
	}
	got := e.findBestAudioStream(formats)
	if got == nil || got.Itag != 141 {
		t.Fatalf("music must never downgrade to a lower direct format to skip deciphering: want 141, got %#v", got)
	}
}

func TestFindBestAudioMusic774CipherBeatsLowerDirect(t *testing.T) {
	e := testMusicSelector()
	formats := []Format{
		testAudioFormat(251, 157841, `audio/webm; codecs="opus"`, "https://x/251.webm", ""),
		testAudioFormat(774, 256000, `audio/mp4; codecs="mp4a.40.2"`, "", "url=http%3A%2F%2Fx%2F774.mp4&s=abc&sp=sig"),
	}
	got := e.findBestAudioStream(formats)
	if got == nil || got.Itag != 774 {
		t.Fatalf("music must rank premium 774 cipher above a lower direct format, got %#v", got)
	}
}

func TestFindBestAudioMusicEqualItagPrefersDirect(t *testing.T) {
	e := testMusicSelector()
	formats := []Format{
		testAudioFormat(140, 133465, `audio/mp4; codecs="mp4a.40.2"`, "", "url=http%3A%2F%2Fx%2F140c.mp4&s=abc&sp=sig"),
		testAudioFormat(140, 133465, `audio/mp4; codecs="mp4a.40.2"`, "https://x/140d.mp4", ""),
	}
	got := e.findBestAudioStream(formats)
	if got == nil || got.URL == "" {
		t.Fatalf("equal quality must prefer the direct URL candidate, got %#v", got)
	}
}

func TestFindBestAudioMusicExplicitM4APrefers140Over251(t *testing.T) {
	e := testMusicSelector()
	e.audioFormat = AudioFormatM4A
	formats := []Format{
		testAudioFormat(140, 133465, `audio/mp4; codecs="mp4a.40.2"`, "https://x/140.mp4", ""),
		testAudioFormat(251, 157841, `audio/webm; codecs="opus"`, "https://x/251.webm", ""),
	}
	got := e.findBestAudioStream(formats)
	if got == nil || got.Itag != 140 {
		t.Fatalf("explicit M4A preference must keep 140 over 251, got %#v", got)
	}
}

func TestFindBestAudioMusicUnknownItagFallsBackToBitrateWithDirectTie(t *testing.T) {
	e := testMusicSelector()
	formats := []Format{
		testAudioFormat(99991, 100000, `audio/mp4; codecs="mp4a.40.2"`, "", "url=http%3A%2F%2Fx%2Fa.mp4&s=abc&sp=sig"),
		testAudioFormat(99992, 120000, `audio/mp4; codecs="mp4a.40.2"`, "https://x/b.mp4", ""),
	}
	if got := e.findBestAudioStream(formats); got == nil || got.Itag != 99992 {
		t.Fatalf("unknown itags must fall back to highest bitrate, got %#v", got)
	}

	tie := []Format{
		testAudioFormat(99993, 100000, `audio/mp4; codecs="mp4a.40.2"`, "", "url=http%3A%2F%2Fx%2Fc.mp4&s=abc&sp=sig"),
		testAudioFormat(99994, 100000, `audio/mp4; codecs="mp4a.40.2"`, "https://x/d.mp4", ""),
	}
	if got := e.findBestAudioStream(tie); got == nil || got.URL == "" {
		t.Fatalf("equal-bitrate unknown itags must prefer the direct candidate, got %#v", got)
	}
}

func TestFindBestAudioMusicNoUsableOrNone(t *testing.T) {
	e := testMusicSelector()

	// Audio entries with neither URL nor signatureCipher are not usable.
	unusable := []Format{
		testAudioFormat(140, 133465, `audio/mp4; codecs="mp4a.40.2"`, "", ""),
		testAudioFormat(251, 157841, `audio/webm; codecs="opus"`, "", ""),
	}
	if got := e.findBestAudioStream(unusable); got != nil {
		t.Fatalf("music must return nil when no usable URL/signatureCipher exists, got %#v", got)
	}

	if got := e.findBestAudioStream(nil); got != nil {
		t.Fatalf("empty formats must return nil, got %#v", got)
	}

	videoOnly := []Format{
		{Itag: 137, Bitrate: 813741, MimeType: `video/mp4; codecs="avc1.640028"`, Height: 1080, URL: "https://x/v.mp4"},
	}
	if got := e.findBestAudioStream(videoOnly); got != nil {
		t.Fatalf("video-only formats must return nil, got %#v", got)
	}

	// A usable entry is picked again once present.
	mixed := append(append([]Format{}, unusable...), testAudioFormat(251, 157841, `audio/webm; codecs="opus"`, "https://x/251.webm", ""))
	if got := e.findBestAudioStream(mixed); got == nil || got.Itag != 251 {
		t.Fatalf("usable candidate must be selected alongside unusable entries, got %#v", got)
	}
}

func TestFindBestAudioMusicMissingURLCipherExcluded(t *testing.T) {
	// A signature cipher without a url value ("s=abc" only) is not a
	// playable candidate: getStreamURLContext would reject it. Selecting
	// the direct 251 here is legitimate unusable-exclusion, not a quality
	// downgrade.
	e := testMusicSelector()
	formats := []Format{
		testAudioFormat(774, 256000, `audio/mp4; codecs="mp4a.40.2"`, "", "s=abc&sp=sig"),
		testAudioFormat(251, 157841, `audio/webm; codecs="opus"`, "https://x/251.webm", ""),
	}
	got := e.findBestAudioStream(formats)
	if got == nil || got.Itag != 251 {
		t.Fatalf("cipher without a url is unusable: must select direct 251, got %#v", got)
	}
}

func TestFindBestAudioMusicMalformedCipherExcluded(t *testing.T) {
	// A cipher with an invalid URL escape does not parse strictly and must
	// not be treated as usable: direct 251 wins for a legitimate reason.
	e := testMusicSelector()
	formats := []Format{
		testAudioFormat(774, 256000, `audio/mp4; codecs="mp4a.40.2"`, "", "url=http%3A%2F%2Fx%2F774.mp4&s=%zz"),
		testAudioFormat(251, 157841, `audio/webm; codecs="opus"`, "https://x/251.webm", ""),
	}
	got := e.findBestAudioStream(formats)
	if got == nil || got.Itag != 251 {
		t.Fatalf("malformed cipher is unusable: must select direct 251, got %#v", got)
	}
}

func TestFindBestAudioMusicOnlyUnusableCiphersNil(t *testing.T) {
	// When every music candidate is unusable (missing url or malformed
	// escapes), selection is nil and the caller retains an actionable
	// error instead of picking a broken stream.
	e := testMusicSelector()
	formats := []Format{
		testAudioFormat(774, 256000, `audio/mp4; codecs="mp4a.40.2"`, "", "s=abc&sp=sig"),
		testAudioFormat(251, 157841, `audio/webm; codecs="opus"`, "", "url=http%3A%2F%2Fx%2F251.webm&s=%zz"),
	}
	if got := e.findBestAudioStream(formats); got != nil {
		t.Fatalf("music must return nil when every cipher is unusable, got %#v", got)
	}
}

func TestFindBestAudioMusicEscapedURLRepeatedKeysUsable(t *testing.T) {
	// Valid escaped URLs and repeated query keys keep url.ParseQuery
	// semantics: the cipher stays usable and still beats a direct 251.
	e := testMusicSelector()
	formats := []Format{
		testAudioFormat(774, 256000, `audio/mp4; codecs="mp4a.40.2"`, "", "url=http%3A%2F%2Fx%2F774.mp4&url=http%3A%2F%2Fx%2Falt.mp4&s=abc&sp=sig"),
		testAudioFormat(251, 157841, `audio/webm; codecs="opus"`, "https://x/251.webm", ""),
	}
	got := e.findBestAudioStream(formats)
	if got == nil || got.Itag != 774 {
		t.Fatalf("escaped-url cipher with repeated keys must stay usable and preferred, got %#v", got)
	}
}

func TestFindBestAudioVideoBothDirect140Before251(t *testing.T) {
	// Legacy video keeps its HEAD priority: with both 140 and 251 direct,
	// 140 (AAC) must still win. The 251-over-140 quality policy is
	// music-only; it must never leak into video mode.
	e := &Extractor{config: GetClientConfig(ModeVideo), audioFormat: AudioFormatWebm}
	formats := []Format{
		testAudioFormat(140, 133465, `audio/mp4; codecs="mp4a.40.2"`, "https://x/140.mp4", ""),
		testAudioFormat(251, 157841, `audio/webm; codecs="opus"`, "https://x/251.webm", ""),
	}
	got := e.findBestAudioStream(formats)
	if got == nil || got.Itag != 140 {
		t.Fatalf("video must keep 140 before 251 for direct formats, got %#v", got)
	}
}

func TestFindBestAudioVideoDirectFirstUnchanged(t *testing.T) {
	// Legacy video chain pre-partitions direct formats, so selection here
	// must keep preferring direct candidates over ciphered premium ones.
	e := &Extractor{config: GetClientConfig(ModeVideo), audioFormat: AudioFormatWebm}
	formats := []Format{
		testAudioFormat(141, 256000, `audio/mp4; codecs="mp4a.40.2"`, "", "url=http%3A%2F%2Fx%2F141.mp4&s=abc&sp=sig"),
		testAudioFormat(140, 133465, `audio/mp4; codecs="mp4a.40.2"`, "https://x/140.mp4", ""),
	}
	got := e.findBestAudioStream(formats)
	if got == nil || got.URL == "" {
		t.Fatalf("video must keep its direct-first contract, got %#v", got)
	}
}
