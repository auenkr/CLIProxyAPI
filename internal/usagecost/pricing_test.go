package usagecost

import "testing"

func TestPricerLookupNormalizesDottedVersions(t *testing.T) {
	p := NewPricer(map[string]Price{"custom.model-1.2": {Input: 7}})
	got, ok := p.Lookup("claude-opus-5.5")
	want, _ := p.Lookup("claude-opus-5-5")
	if !ok || got != want {
		t.Fatalf("dotted opus 5.5 = %+v (ok=%v), want %+v", got, ok, want)
	}
	if price, ok := p.Lookup("Custom.Model-1.2"); !ok || price.Input != 7 {
		t.Fatalf("dotted override = %+v (ok=%v)", price, ok)
	}
}
