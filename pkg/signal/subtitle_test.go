package signal

import "testing"

func TestIsCaptionPresent(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		// Real OCR reads from the gow_video3 720p proxy caption band.
		{"full dialogue line", "Heiidad: You knaw that thing tears holes between realms, right?", true},
		{"speaker plus sentence", "Heinen Yes. yes. The durnib dees not fall far from the tree", true},
		{"captionless colon noise", ":", false},
		{"captionless punctuation noise", ": | 7 :", false},
		{"empty", "", false},
		{"two short words below bar", "ok go", false},
	}
	for _, c := range cases {
		if got := isCaptionPresent(c.text); got != c.want {
			t.Errorf("%s: isCaptionPresent(%q) = %v, want %v", c.name, c.text, got, c.want)
		}
	}
}
