package signal

import "testing"

// A 720p frame letterboxed with 90px bars (12.5% each), symmetric: y1=90,
// y2=629 (bottom bar = 720-1-629 = 90). Non-letterbox frame fills the frame.
func cropLine(t float64, y1, y2 int) string {
	return "[Parsed_cropdetect_1 @ 0x0] x1:0 x2:1279 y1:" + itoa(y1) +
		" y2:" + itoa(y2) + " w:1280 h:" + itoa(y2-y1+1) +
		" x:0 y:" + itoa(y1) + " pts:1 t:" + ftoa(t) + " limit:24.000000 crop=1280:0:0:0\n"
}

func TestParseCropdetect_SymmetricBarsAreLetterboxed(t *testing.T) {
	log := cropLine(1.0, 90, 629) + // 90px top & bottom bars -> cutscene
		cropLine(1.5, 0, 719) // full frame -> gameplay
	got := ParseCropdetect(log, 720)
	if len(got) != 2 {
		t.Fatalf("want 2 samples, got %d", len(got))
	}
	if !got[0].Active {
		t.Errorf("symmetric 90px bars should be letterboxed")
	}
	if got[1].Active {
		t.Errorf("full-frame sample should not be letterboxed")
	}
}

func TestParseCropdetect_AsymmetricBarIsNotLetterbox(t *testing.T) {
	// Bottom-only dark region (a HUD panel), no top bar: y1=0, y2=629.
	log := cropLine(1.0, 0, 629)
	got := ParseCropdetect(log, 720)
	if len(got) != 1 || got[0].Active {
		t.Errorf("one-sided dark region should not be letterboxed: %+v", got)
	}
}

func TestParseCropdetect_TinyBarsBelowThreshold(t *testing.T) {
	// 10px bars (~1.4%) are below the 6% minimum — encoding noise, not cinema.
	log := cropLine(1.0, 10, 709)
	got := ParseCropdetect(log, 720)
	if len(got) != 1 || got[0].Active {
		t.Errorf("sub-threshold bars should not be letterboxed: %+v", got)
	}
}

func TestGroupCutscenes_MergesBridgesAndFiltersShort(t *testing.T) {
	samples := []PresenceSample{
		{T: 0.0, Active: false},
		{T: 10.0, Active: true}, // cutscene A start
		{T: 11.0, Active: true},
		{T: 13.0, Active: true}, // 2.0s gap <= maxGap, still A (len 3.0)
		{T: 20.0, Active: true}, // 7s gap > maxGap -> cutscene B start
		{T: 21.0, Active: true},
		{T: 22.0, Active: true},
		{T: 24.0, Active: true}, // B end (len 4.0)
		{T: 40.0, Active: true}, // lone 0-length blip -> filtered
	}
	segs := GroupCutscenes(samples, 3.0, 2.0)
	if len(segs) != 2 {
		t.Fatalf("want 2 segments, got %d: %+v", len(segs), segs)
	}
	if segs[0].Start != 10.0 || segs[0].End != 13.0 {
		t.Errorf("segment A wrong: %+v", segs[0])
	}
	if segs[1].Start != 20.0 || segs[1].End != 24.0 {
		t.Errorf("segment B wrong: %+v", segs[1])
	}
}

func TestGroupCutscenes_GapBreaksSegment(t *testing.T) {
	// 5s gap (> maxGap 2s) splits into two, but each < minDur -> both dropped.
	samples := []PresenceSample{
		{T: 10.0, Active: true},
		{T: 11.0, Active: true},
		{T: 16.0, Active: true},
		{T: 17.0, Active: true},
	}
	segs := GroupCutscenes(samples, 3.0, 2.0)
	if len(segs) != 0 {
		t.Errorf("want 0 segments (each run too short after gap split), got %+v", segs)
	}
}

func TestEmitTimes_SpansCutscene(t *testing.T) {
	times := emitTimes(Segment{Start: 100, End: 110}, 4.0)
	// start(100), +4(104), +4(108), end(110)
	want := []float64{100, 104, 108, 110}
	if len(times) != len(want) {
		t.Fatalf("want %v, got %v", want, times)
	}
	for i := range want {
		if times[i] != want[i] {
			t.Errorf("emit time %d: want %v got %v", i, want[i], times[i])
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func ftoa(f float64) string {
	// Test timestamps are simple 1-decimal values; format minimally.
	whole := int(f)
	frac := int((f-float64(whole))*10 + 0.5)
	return itoa(whole) + "." + itoa(frac) + "00000"
}
