package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSchedulingFallbackPreservesFreeSite(t *testing.T) {
	for _, binding := range []string{"eligible", "cooldown", "attempted", "removed", "busy_free"} {
		t.Run(binding, func(t *testing.T) {
			isolatePolicies(t)
			setSchedulingConfig(schedulingFileConfig{WaitTimeoutMs: 1000})
			const model = "synthetic-scheduling-free-site"
			modelsMu.Lock()
			oldCatalogs, oldProbes := catalogModels, modelProbes
			catalogModels = map[string][]catalogModel{
				"cn":   {{ID: model, Multiplier: 1, HasMultiplier: true, FromLive: true}},
				"intl": {{ID: model, Multiplier: 0, HasMultiplier: true, FromLive: true}},
			}
			modelProbes = map[string]modelPriceProbe{}
			modelsMu.Unlock()
			t.Cleanup(func() {
				modelsMu.Lock()
				catalogModels, modelProbes = oldCatalogs, oldProbes
				modelsMu.Unlock()
			})
			paid, free := mkAccount("synthetic-paid-cn.json"), mkAccount("synthetic-free-intl.json")
			free.Auth.Edition = "intl"
			var attempted map[*Account]bool
			pool := []*Account{paid, free}
			switch binding {
			case "cooldown":
				paid.CooldownUntil = time.Now().Add(time.Minute)
			case "attempted":
				attempted = map[*Account]bool{paid: true}
			case "removed":
				pool = []*Account{free}
			}
			withAccounts(t, pool...)
			if binding == "busy_free" {
				modelConcurrency.configure(concurrentFileConfig{MaxAcc: 1})
				held, _, err := modelConcurrency.tryAcquire(context.Background(), modelConcurrencyKey(free))
				if err != nil || held == nil {
					t.Fatal("failed to occupy free account")
				}
				defer held()
				setSchedulingConfig(schedulingFileConfig{AffinityWaitMs: 10, WaitTimeoutMs: 80})
			}
			if preferred := preferredFreeSites(model); !preferred["intl"] || preferred["cn"] {
				t.Fatalf("invalid free-site fixture: %v", preferred)
			}
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			selected, _, release, err := acquireScheduledAccount(r, model, attempted, paid.Path, 1, "")
			if binding == "busy_free" {
				if release != nil {
					release()
				}
				if !errors.Is(err, errSchedulingTimeout) || selected != nil {
					t.Fatalf("busy free site spilled to paid account: selected=%v err=%v", selected, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if selected != free {
				t.Fatalf("affinity fallback bypassed free site: got %s", selected.Path)
			}
		})
	}
}

func TestSchedulingSkipsMissingCredentials(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		name := "all_missing"
		if healthy {
			name = "healthy_fallback"
		}
		t.Run(name, func(t *testing.T) {
			isolatePolicies(t)
			setSchedulingConfig(schedulingFileConfig{WaitTimeoutMs: 80})
			bad, good := mkAccount("synthetic-missing-auth.json"), mkAccount("synthetic-healthy-auth.json")
			bad.Auth = nil
			pool := []*Account{bad}
			if healthy {
				pool = append(pool, good)
			}
			withAccounts(t, pool...)
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			selected, _, release, err := acquireScheduledAccount(r, "synthetic-auth-model", nil, "", 1, "")
			if release != nil {
				release()
			}
			if errors.Is(err, errSchedulingTimeout) {
				t.Fatalf("missing credentials caused scheduling to spin until timeout: %v", err)
			}
			if healthy && (err != nil || selected != good) {
				t.Fatalf("healthy fallback failed: selected=%v err=%v", selected, err)
			}
			if !healthy && err == nil {
				t.Fatal("account without credentials was admitted")
			}
			_, total, _ := modelConcurrency.snapshot("")
			if total != 0 {
				t.Fatalf("invalid account leaked %d model permits", total)
			}
		})
	}
}

func TestSchedulingFixtureStartsWithDefaultsAndRestores(t *testing.T) {
	old := currentSchedulingConfig()
	t.Cleanup(func() { setSchedulingConfig(old) })
	baseline := schedulingFileConfig{WaitTimeoutMs: 987, AffinityWaitMs: 456}
	setSchedulingConfig(baseline)
	t.Run("isolated", func(t *testing.T) {
		isolatePolicies(t)
		if got := currentSchedulingConfig(); got != (schedulingFileConfig{}) {
			t.Fatalf("fixture inherited previous scheduling settings: %+v", got)
		}
		setSchedulingConfig(schedulingFileConfig{WaitTimeoutMs: 25})
	})
	if got := currentSchedulingConfig(); got != baseline {
		t.Fatalf("fixture did not restore scheduling settings: %+v", got)
	}
}
