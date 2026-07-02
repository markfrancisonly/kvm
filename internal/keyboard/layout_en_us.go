package keyboard

// charCombo describes how to type one character: the key to press and the
// layout modifiers to hold. Mirrors the KeyCombo entries in
// ui/src/keyboardLayouts (only the fields the en-US layout uses).
type charCombo struct {
	key      string
	shift    bool
	altRight bool
}

// charsForLayout returns the character table for a keyboard layout, or false
// if text input is not supported for it on the device yet. The remaining
// layouts from ui/src/keyboardLayouts need dead-key/accent-key handling and
// are left as a follow-up.
func charsForLayout(layout string) (map[rune]charCombo, bool) {
	switch layout {
	case "", "en-US", "en_US":
		return enUSChars, true
	}
	return nil, false
}

// enUSChars maps a character to the key combo that types it on the en-US
// layout. Ported from ui/src/keyboardLayouts/en_US.ts `export const chars`
// (single-character entries only), plus '\t' for Tab.
var enUSChars = map[rune]charCombo{
	'A':  {key: "KeyA", shift: true},
	'B':  {key: "KeyB", shift: true},
	'C':  {key: "KeyC", shift: true},
	'D':  {key: "KeyD", shift: true},
	'E':  {key: "KeyE", shift: true},
	'F':  {key: "KeyF", shift: true},
	'G':  {key: "KeyG", shift: true},
	'H':  {key: "KeyH", shift: true},
	'I':  {key: "KeyI", shift: true},
	'J':  {key: "KeyJ", shift: true},
	'K':  {key: "KeyK", shift: true},
	'L':  {key: "KeyL", shift: true},
	'M':  {key: "KeyM", shift: true},
	'N':  {key: "KeyN", shift: true},
	'O':  {key: "KeyO", shift: true},
	'P':  {key: "KeyP", shift: true},
	'Q':  {key: "KeyQ", shift: true},
	'R':  {key: "KeyR", shift: true},
	'S':  {key: "KeyS", shift: true},
	'T':  {key: "KeyT", shift: true},
	'U':  {key: "KeyU", shift: true},
	'V':  {key: "KeyV", shift: true},
	'W':  {key: "KeyW", shift: true},
	'X':  {key: "KeyX", shift: true},
	'Y':  {key: "KeyY", shift: true},
	'Z':  {key: "KeyZ", shift: true},
	'a':  {key: "KeyA"},
	'b':  {key: "KeyB"},
	'c':  {key: "KeyC"},
	'd':  {key: "KeyD"},
	'e':  {key: "KeyE"},
	'f':  {key: "KeyF"},
	'g':  {key: "KeyG"},
	'h':  {key: "KeyH"},
	'i':  {key: "KeyI"},
	'j':  {key: "KeyJ"},
	'k':  {key: "KeyK"},
	'l':  {key: "KeyL"},
	'm':  {key: "KeyM"},
	'n':  {key: "KeyN"},
	'o':  {key: "KeyO"},
	'p':  {key: "KeyP"},
	'q':  {key: "KeyQ"},
	'r':  {key: "KeyR"},
	's':  {key: "KeyS"},
	't':  {key: "KeyT"},
	'u':  {key: "KeyU"},
	'v':  {key: "KeyV"},
	'w':  {key: "KeyW"},
	'x':  {key: "KeyX"},
	'y':  {key: "KeyY"},
	'z':  {key: "KeyZ"},
	'1':  {key: "Digit1"},
	'!':  {key: "Digit1", shift: true},
	'2':  {key: "Digit2"},
	'@':  {key: "Digit2", shift: true},
	'3':  {key: "Digit3"},
	'#':  {key: "Digit3", shift: true},
	'4':  {key: "Digit4"},
	'$':  {key: "Digit4", shift: true},
	'5':  {key: "Digit5"},
	'%':  {key: "Digit5", shift: true},
	'6':  {key: "Digit6"},
	'^':  {key: "Digit6", shift: true},
	'7':  {key: "Digit7"},
	'&':  {key: "Digit7", shift: true},
	'8':  {key: "Digit8"},
	'*':  {key: "Digit8", shift: true},
	'9':  {key: "Digit9"},
	'(':  {key: "Digit9", shift: true},
	'0':  {key: "Digit0"},
	')':  {key: "Digit0", shift: true},
	'-':  {key: "Minus"},
	'_':  {key: "Minus", shift: true},
	'=':  {key: "Equal"},
	'+':  {key: "Equal", shift: true},
	'\'': {key: "Quote"},
	'"':  {key: "Quote", shift: true},
	',':  {key: "Comma"},
	'<':  {key: "Comma", shift: true},
	'/':  {key: "Slash"},
	'?':  {key: "Slash", shift: true},
	'.':  {key: "Period"},
	'>':  {key: "Period", shift: true},
	';':  {key: "Semicolon"},
	':':  {key: "Semicolon", shift: true},
	'¶':  {key: "Semicolon", altRight: true}, // pilcrow sign
	'[':  {key: "BracketLeft"},
	'{':  {key: "BracketLeft", shift: true},
	'«':  {key: "BracketLeft", altRight: true}, // double left quote sign
	']':  {key: "BracketRight"},
	'}':  {key: "BracketRight", shift: true},
	'»':  {key: "BracketRight", altRight: true}, // double right quote sign
	'\\': {key: "Backslash"},
	'|':  {key: "Backslash", shift: true},
	'¬':  {key: "Backslash", altRight: true}, // not sign
	'`':  {key: "Backquote"},
	'~':  {key: "Backquote", shift: true},
	'§':  {key: "IntlBackslash"},
	'±':  {key: "IntlBackslash", shift: true},
	' ':  {key: "Space"},
	'\n': {key: "Enter"},
	'\t': {key: "Tab"},
}
