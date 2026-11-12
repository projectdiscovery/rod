package rod

import "testing"

func TestCompliantFromEval(t *testing.T) {
	raw := `{
		"ua": "Mozilla/5.0 HeadlessChrome/157.0.0.0 Safari/537.36",
		"hints": {
			"architecture": "arm",
			"bitness": "64",
			"brands": [
				{"brand": "Not-A.Brand", "version": "99"},
				{"brand": "HeadlessChrome", "version": "157"}
			],
			"fullVersionList": [
				{"brand": "Not-A.Brand", "version": "99.0.0.0"},
				{"brand": "HeadlessChrome", "version": "157.0.8103.39"}
			],
			"platform": "macOS",
			"platformVersion": "27.2.0",
			"formFactors": ["Desktop"]
		}
	}`
	ua, meta, err := compliantFromEval(raw)
	if err != nil {
		t.Fatal(err)
	}
	if ua != "Mozilla/5.0 Chrome/157.0.0.0 Safari/537.36" {
		t.Fatalf("ua %s", ua)
	}
	if meta.Platform != "macOS" || meta.PlatformVersion != "27.2.0" || meta.Architecture != "arm" {
		t.Fatalf("meta lost host values: %+v", meta)
	}
	if meta.Brands[1].Brand != "Google Chrome" || meta.Brands[1].Version != "157" {
		t.Fatalf("brand %+v", meta.Brands[1])
	}
	if meta.FullVersionList[1].Version != "157.0.8103.39" {
		t.Fatalf("full version %s", meta.FullVersionList[1].Version)
	}
	if meta.Brands[0] == meta.Brands[1] {
		t.Fatal("brand pointers alias")
	}

	if _, meta, err = compliantFromEval(`{"ua":"x"}`); err != nil || meta != nil {
		t.Fatalf("incomplete hints should wait, got %v %v", meta, err)
	}
}
