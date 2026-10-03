package cutter

import "testing"

func TestShortCanvasKeepsSourceDetail(t *testing.T) {
	cases := []struct {
		name         string
		srcW, srcH   int
		override     int
		wantW, wantH int
	}{
		// The case that motivated this: a 4K master used to be squeezed onto a
		// fixed 1080x1920 canvas, downscaling the gameplay strip 3.6x.
		{name: "4k source", srcW: 3840, srcH: 2160, wantW: 2160, wantH: 3840},
		// 1080p in, 1080x1920 out — the old hardcoded behaviour, now derived.
		{name: "1080p source", srcW: 1920, srcH: 1080, wantW: 1080, wantH: 1920},
		{name: "1440p source", srcW: 2560, srcH: 1440, wantW: 1440, wantH: 2560},
		// An already-vertical source must not be upscaled to its own height.
		{name: "vertical source", srcW: 1080, srcH: 1920, wantW: 1080, wantH: 1920},
		// An override pins the height and derives 9:16 width from it.
		{name: "override", srcW: 3840, srcH: 2160, override: 1920, wantW: 1080, wantH: 1920},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h := shortCanvas(tc.srcW, tc.srcH, tc.override)
			if w != tc.wantW || h != tc.wantH {
				t.Errorf("shortCanvas(%d, %d, %d) = %dx%d, want %dx%d",
					tc.srcW, tc.srcH, tc.override, w, h, tc.wantW, tc.wantH)
			}
		})
	}
}

func TestShortCanvasAlwaysEven(t *testing.T) {
	// libx264 rejects odd dimensions under yuv420p, and the source's short side
	// is not guaranteed to be even (nor is 9/16 of an arbitrary override).
	for _, dims := range [][3]int{{1281, 721, 0}, {999, 555, 0}, {0, 0, 1081}, {0, 0, 3}} {
		w, h := shortCanvas(dims[0], dims[1], dims[2])
		if w%2 != 0 || h%2 != 0 || w < 2 || h < 2 {
			t.Errorf("shortCanvas(%d, %d, %d) = %dx%d, want positive even dimensions",
				dims[0], dims[1], dims[2], w, h)
		}
	}
}

func TestParseFormats(t *testing.T) {
	got, err := ParseFormats([]string{" Clip ", "short", "clip", ""})
	if err != nil {
		t.Fatalf("ParseFormats: %v", err)
	}
	if len(got) != 2 || got[0] != FormatClip || got[1] != FormatShort {
		t.Errorf("got %v, want [clip short] deduped and in order", got)
	}

	if _, err := ParseFormats([]string{"gif"}); err == nil {
		t.Error("expected an error for an unknown format")
	}
	if _, err := ParseFormats([]string{"", "  "}); err == nil {
		t.Error("expected an error when nothing at all was selected")
	}
}

func TestVideoName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// The everyday case: the recording's own name becomes the directory.
		{"raw_clips/gow/gow_video05.webm", "gow_video05"},
		{"gow_dummy.mp4", "gow_dummy"},
		{"/abs/path/valo_02.mp4", "valo_02"},
		// Capture-tool names: spaces, punctuation and non-ASCII collapse to
		// single underscores rather than a path that is painful to type.
		{"raw_clips/mk_1/Mortal Kombat™ 1_20260704141421.webm", "Mortal_Kombat_1_20260704141421"},
		// A Windows path arriving through WSL: only the last segment is the name.
		{`C:\Users\prith\Videos\gow_video05.webm`, "gow_video05"},
		// A leading dot must not turn the output into a hidden directory.
		{".hidden.webm", "hidden"},
		// Nothing usable left — fall back rather than write into out/<game>/.
		{"...", "unknown"},
		{"", "unknown"},
	}
	for _, tc := range cases {
		if got := VideoName(tc.in); got != tc.want {
			t.Errorf("VideoName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOutDirSeparatesRecordings(t *testing.T) {
	// The bug this layout fixes: two recordings of the same game both produce a
	// clip_01, and under the old out/<game>/<clip> layout the second overwrote
	// the first.
	a := OutDir("gow", "raw_clips/gow/gow_video05.webm")
	b := OutDir("gow", "raw_clips/gow/gow_video03.webm")
	if a == b {
		t.Errorf("two recordings of the same game share an output dir: %s", a)
	}
	if want := "out/gow/gow_video05"; a != want {
		t.Errorf("OutDir = %q, want %q", a, want)
	}
}

func TestRenderOptsDefaultsAndWants(t *testing.T) {
	var zero RenderOpts // a caller that only set Formats
	if zero.crf() != "18" || zero.preset() != "slow" {
		t.Errorf("zero RenderOpts = crf %s preset %s, want the package defaults", zero.crf(), zero.preset())
	}

	only := DefaultRenderOpts().WithFormats([]Format{FormatShort})
	if only.wants(FormatClip) || !only.wants(FormatShort) {
		t.Errorf("WithFormats([short]) should want short and not clip, got %v", only.Formats)
	}
	if only.crf() != DefaultRenderOpts().crf() {
		t.Error("WithFormats must not disturb the encode settings")
	}
}
