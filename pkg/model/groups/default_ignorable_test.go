package groups

import "testing"

// The display escaper now escapes every default-ignorable code point (2026-11-19), so a name hiding a
// selector-encoded payload after one visible letter shows as escapes in `group list` and rudolph-web. A new group
// name must not need escaping at all (owner decision, 2026-11-19): among the default-ignorables only ZWNJ and ZWJ
// stay, because Persian and Indic spelling needs them. U+FE0F is refused too, even though the display keeps it, since
// a group name has no use for emoji presentation. The blank-looking fillers (U+3164, U+2800 and the like) are not all
// default-ignorable, but a name of them looks empty, so they are refused as well.
//
// Each character is built from its number: written literally it would be invisible here too.
var refusedInvisible = []rune{
	0xfe00, 0xfe0e, 0xfe0f, // variation selectors, text and emoji presentation
	0xe0100, 0xe01ef, // supplementary variation selectors
	0x034f,  // combining grapheme joiner
	0x180b,  // Mongolian free variation selector
	0x17b4,  // Khmer inherent vowel
	0x2065,  // reserved default-ignorable
	0xfff0,  // reserved default-ignorable
	0x3164,  // Hangul filler, blank-looking
	0x2800,  // braille blank, blank-looking
	0xe0041, // tag letter A
}

func hiddenAfter(prefix string, r rune) string { return prefix + string(r) }

func TestNormalise_RefusesDefaultIgnorableAndBlankCharacters(t *testing.T) {
	for _, r := range refusedInvisible {
		for _, name := range []string{
			hiddenAfter("e", r) + "ng",             // a payload after one visible letter
			hiddenAfter("eng", r),                  // trailing
			string(r) + "eng",                      // leading
			hiddenAfter("e", r) + string(r) + "ng", // more than one
		} {
			if got, err := Normalise(name); err == nil {
				t.Errorf("Normalise(%+q) with U+%04X = %+q, want an error", name, r, got)
			}
			if got, err := NormaliseTarget(name); err == nil {
				t.Errorf("NormaliseTarget(%+q) with U+%04X = %+q, want an error", name, r, got)
			}
		}
	}
}

func TestNormalise_StillAcceptsJoinersAccentsAndCJK(t *testing.T) {
	zwnj, zwj := string(rune(0x200c)), string(rune(0x200d))
	for _, name := range []string{
		"می" + zwnj + "خواهم", // Persian
		"क्" + zwj + "ष",      // Devanagari
		"ක්" + zwj + "ර",      // Sinhala
		"eng" + zwnj + "ops",
		"jürgen",
		"e" + string(rune(0x0301)) + "quipe", // a combining accent, composed by NFC
		"工程",
		"한국-팀",
		"lab 2",
	} {
		if _, err := Normalise(name); err != nil {
			t.Errorf("Normalise(%+q): %v, want it accepted", name, err)
		}
	}
}

// A Mac reports its directory groups in preflight; a reported name with one of these characters must be dropped, so
// it never becomes a membership or appears in rudolph-web as a group to create.
func TestNormaliseList_DropsReportedNamesWithDefaultIgnorableCharacters(t *testing.T) {
	reported := []string{"eng"}
	for _, r := range refusedInvisible {
		reported = append(reported, hiddenAfter("o", r)+"ps")
	}
	got, dropped := NormaliseList(reported)
	if len(got) != 1 || got[0] != "eng" || dropped != len(refusedInvisible) {
		t.Errorf("NormaliseList = %+q, dropped %d; want [eng], %d", got, dropped, len(refusedInvisible))
	}
}

// Groups registered before this rule may have such names; like the Cf names refused since 2026-11-18, they must stay
// manageable (shown, emptied and deleted) by the name they were registered with.
func TestNormaliseExisting_StillAcceptsRegisteredNamesWithDefaultIgnorableCharacters(t *testing.T) {
	for _, r := range refusedInvisible {
		name := hiddenAfter("o", r) + "ps"
		for fn, normalise := range map[string]func(string) (string, error){
			"NormaliseExisting": NormaliseExisting, "NormaliseExistingTarget": NormaliseExistingTarget,
		} {
			got, err := normalise(name)
			if err != nil {
				t.Errorf("%s(%+q) with U+%04X: %v, want it accepted", fn, name, r, err)
				continue
			}
			if got != name {
				t.Errorf("%s(%+q) = %+q, want the name unchanged", fn, name, got)
			}
		}
	}
	if got, err := NormaliseExistingTarget(Ungrouped); err != nil || got != Ungrouped {
		t.Errorf("NormaliseExistingTarget(Ungrouped) = %+q, %v", got, err)
	}
}
