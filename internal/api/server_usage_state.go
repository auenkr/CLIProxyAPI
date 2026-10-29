package api

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/statemirror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagecost"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageaggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	usageStateMirrorKey = "usage.state"
	usageCostMirrorKey  = "usage-cost.state"
)

// tokenAggregateSource adapts the process-wide token aggregate to a
// usagestate.TokensSource.
type tokenAggregateSource struct{}

func (tokenAggregateSource) Revision() uint64 { return usageaggregate.Revision() }

func (tokenAggregateSource) Snapshot() usageaggregate.UsageSnapshot {
	return usageaggregate.Snapshot()
}

func (tokenAggregateSource) Restore(snapshot usageaggregate.UsageSnapshot) {
	usageaggregate.Restore(snapshot)
}

// newUsageState builds the single-file usage-state persistence manager. It
// returns nil when persistence is unavailable so callers can skip it entirely.
func newUsageState(cfg *config.Config, authManager *coreauth.Manager) *usagestate.Manager {
	if cfg == nil || authManager == nil {
		return nil
	}
	authDir, errResolve := util.ResolveAuthDir(cfg.AuthDir)
	if errResolve != nil || authDir == "" {
		log.Warnf("usage state persistence disabled: resolve auth dir: %v", errResolve)
		return nil
	}

	manager := usagestate.New(
		usagestate.UsageStatePath(filepath.Clean(authDir)),
		usagestate.DefaultInterval,
		authManager,
		tokenAggregateSource{},
	)
	if mirror := statemirror.Default(); mirror != nil {
		manager.SetMirror(mirror, usageStateMirrorKey)
	}
	if errLoad := manager.Load(); errLoad != nil {
		log.Warnf("failed to load persisted usage state: %v", errLoad)
	}
	return manager
}

// newUsageCost builds the API-equivalent cost store and registers it as a usage
// plugin. Price overrides come from model-pricing.json beside the config file.
func newUsageCost(configFilePath string) *usagecost.Store {
	path, pricingPath := "", ""
	if configFilePath != "" {
		dir := filepath.Dir(configFilePath)
		path = filepath.Join(dir, "usage-cost.state")
		pricingPath = filepath.Join(dir, "model-pricing.json")
	}
	mirror := statemirror.Default()
	if mirror != nil && path != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		errRestore := statemirror.Restore(ctx, mirror, usageCostMirrorKey, path)
		cancel()
		if errRestore != nil {
			log.Warnf("usage cost: restore from mirror: %v", errRestore)
		}
	}
	overrides, errLoad := usagecost.LoadOverrides(pricingPath)
	if errLoad != nil {
		log.Warnf("usage cost: load %s: %v", pricingPath, errLoad)
	}
	store := usagecost.New(path, usagecost.NewPricer(overrides))
	if mirror != nil && path != "" {
		store.SetMirror(mirror, usageCostMirrorKey)
	}
	coreusage.RegisterPlugin(store)
	return store
}
