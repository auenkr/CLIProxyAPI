package cliproxy

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"testing"
)

func TestSoonestResetRoutingWiring(t *testing.T) {
	for _, alias := range []string{"soonest-reset", "soonestreset", "sr"} {
		for _, affinity := range []bool{false, true} {
			cfg := &config.Config{}
			cfg.Routing.Strategy = alias
			cfg.Routing.SessionAffinity = affinity
			state := normalizedRoutingRuntimeState(cfg)
			if state.strategy != "soonest-reset" {
				t.Fatalf("alias %s = %s", alias, state.strategy)
			}
			selector := newRoutingSelector(state)
			if affinity {
				if _, ok := selector.(*coreauth.SessionAffinitySelector); !ok {
					t.Fatalf("selector = %T", selector)
				}
			} else if _, ok := selector.(*coreauth.SoonestResetSelector); !ok {
				t.Fatalf("selector = %T", selector)
			}
		}
	}
}
