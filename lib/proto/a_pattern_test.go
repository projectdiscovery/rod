package proto

import (
	"regexp"
	"testing"
)

func TestPatternToRegEscapes(t *testing.T) {
	cases := map[string]string{
		"*/a+b/*":  `\A.*/a\+b/.*\z`,
		"*":        `\A.*\z`,
		`a\*b`:     `\Aa\*b\z`,
		`a\?b`:     `\Aa\?b\z`,
		`trail\`:   `\Atrail\\\z`,
		"(x)|[y]$": `\A\(x\)\|\[y\]\$\z`,
		"héllo/*":  `\Ahéllo/.*\z`,
	}
	for in, want := range cases {
		if got := PatternToReg(in); got != want {
			t.Fatalf("PatternToReg(%q) = %s, want %s", in, got, want)
		}
		if _, err := regexp.Compile(PatternToReg(in)); err != nil {
			t.Fatalf("PatternToReg(%q) does not compile: %v", in, err)
		}
	}

	re := regexp.MustCompile(PatternToReg("https://*.example.com/a?c"))
	if !re.MatchString("https://x.example.com/abc") || re.MatchString("https://x.example.com/abbc") {
		t.Fatal("wildcards")
	}
	if regexp.MustCompile(PatternToReg("a.com")).MatchString("aXcom") {
		t.Fatal("dot must be literal")
	}
}
