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

func TestPricerDocsRates(t *testing.T) {
	p := NewPricer(nil)
	for model, input := range map[string]float64{
		"gpt-6.1-sol": 2, "gpt-6-sol": 2, "gpt-6.1-terra": 1, "gpt-6-astra-law": 10,
		"gpt-6-luna": 0.1, "gpt-6-cyber": 20, "claude-haiku-5-5": 0.1,
	} {
		if got, ok := p.Lookup(model); !ok || got.Input != input {
			t.Errorf("%s input = %v (ok=%v), want %v", model, got.Input, ok, input)
		}
	}
	if got, _ := p.Lookup("gpt-6.1-sol"); got.CacheRead != 0.1 {
		t.Errorf("gpt-6.1-sol cache read = %v", got.CacheRead)
	}
	if got, _ := p.Lookup("claude-sonnet-5-5"); got.CacheRead != 0.1 {
		t.Errorf("sonnet 5.5 cache read = %v", got.CacheRead)
	}
	if got, _ := p.Lookup("claude-fable-5"); got.CacheRead != 1 {
		t.Errorf("fable 5 cache read = %v", got.CacheRead)
	}
}
