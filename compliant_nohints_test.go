package rod

import "testing"

// A build without navigator.userAgentData, such as chrome-headless-shell,
// reports only the user agent. The token must still be stripped and no
// metadata is returned.
func TestCompliantFromEvalNoHints(t *testing.T) {
	ua, meta, err := compliantFromEval(`{"ua":"Mozilla/5.0 HeadlessChrome/157.0.0.0 Safari/537.36"}`)
	if err != nil {
		t.Fatal(err)
	}
	if meta != nil {
		t.Fatalf("meta = %v, want nil", meta)
	}
	if want := "Mozilla/5.0 Chrome/157.0.0.0 Safari/537.36"; ua != want {
		t.Fatalf("ua = %q, want %q", ua, want)
	}
}

// An empty user agent is the page not being ready yet. The caller retries it.
func TestCompliantFromEvalNotReady(t *testing.T) {
	ua, meta, err := compliantFromEval(`{"ua":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if ua != "" || meta != nil {
		t.Fatalf("ua = %q, meta = %v", ua, meta)
	}
}
