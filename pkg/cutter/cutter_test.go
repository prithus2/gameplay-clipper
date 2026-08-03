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
