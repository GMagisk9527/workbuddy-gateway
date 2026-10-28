package main

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

type statusSnapshotAccount struct {
	*Account
	checkin checkinTarget
}

// Capture only the fields used by status/model presentation. Never copy the
// live credential mutex or expose mutable maps/slices after accountMu unlocks.
func captureStatusSnapshotAccounts() []statusSnapshotAccount {
	accountMu.Lock()
	defer accountMu.Unlock()
	captureAccountStatsLocked()
	out := make([]statusSnapshotAccount, 0, len(accounts))
	for _, a := range accounts {
		c := &Account{Path: a.Path, CooldownUntil: a.CooldownUntil, CooldownMsg: a.CooldownMsg,
			Disabled: a.Disabled, DisabledReason: a.DisabledReason, Nickname: a.Nickname, UID: a.UID, Edition: a.Edition,
			QuotaTotal: a.QuotaTotal, QuotaUsed: a.QuotaUsed, QuotaRemaining: a.QuotaRemaining,
			IsPaidUser: a.IsPaidUser, PlanLabel: a.PlanLabel, PlanCheckedAt: a.PlanCheckedAt, PlanStale: a.PlanStale,
			QuotaKnown: a.QuotaKnown, QuotaExhausted: a.QuotaExhausted, QuotaUnlimited: a.QuotaUnlimited,
			RefreshFailCount: a.RefreshFailCount, credits: a.credits}
		if a.Auth != nil {
			c.Auth = &StoredAuth{Account: a.Auth.Account, Edition: a.Auth.Edition,
				Auth: StoredTokens{ExpiresAt: a.Auth.Auth.ExpiresAt, RefreshExpiresAt: a.Auth.Auth.RefreshExpiresAt,
					LastRefreshTime: a.Auth.Auth.LastRefreshTime, RefreshToken: a.Auth.Auth.RefreshToken}}
		}
		c.credits.Packages = slices.Clone(a.credits.Packages)
		if a.ModelStates != nil {
			c.ModelStates = make(map[string]*modelRuntimeState, len(a.ModelStates))
			for model, state := range a.ModelStates {
				if state != nil {
					copy := *state
					c.ModelStates[model] = &copy
				}
			}
		}
		// Eligibility depends on the live AccessToken, which the presentation
		// copy deliberately omits. Keep only the derived identity and flags;
		// view never needs the source account or permission to dispatch work.
		checkin := checkinTargetLocked(a)
		checkin.account = nil
		out = append(out, statusSnapshotAccount{Account: c, checkin: checkin})
	}
	return out
}

// The management UI considers snapshots stale after 15s. Keep an unchanged
// status heartbeat at 9s, with the existing 3s reevaluation cadence retained.
const statusSnapshotHeartbeat = 9 * time.Second

// Protected by statusSnapshotWriteMu. Cache only the last successful write;
// failed writes, a different work directory, or a changed/deleted file retry.
var statusSnapshotSaved struct {
	path string
	hash [sha256.Size]byte
	at   time.Time
	info os.FileInfo
}

func persistStatusSnapshot(snap statusSnapshot, now time.Time) error {
	path, err := filepath.Abs(statusSnapshotFile)
	if err != nil {
		return err
	}
	canonical := snap
	canonical.UpdatedAt = 0
	canonical.Models = slices.Clone(snap.Models)
	canonical.ModelCatalog = slices.Clone(snap.ModelCatalog)
	byID := func(rows []modelStatSnapshot) {
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	}
	byID(canonical.Models)
	byID(canonical.ModelCatalog)
	// Alert maps have no iteration order; stabilize their small arrays as well.
	for i := range snap.Accounts {
		sort.Slice(snap.Accounts[i].Alerts, func(a, b int) bool { return snap.Accounts[i].Alerts[a].State < snap.Accounts[i].Alerts[b].State })
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(raw)
	last := &statusSnapshotSaved
	age := now.Sub(last.at)
	if last.path == path && last.hash == hash && age >= 0 && age < statusSnapshotHeartbeat && last.info != nil {
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && os.SameFile(last.info, info) && info.Size() == last.info.Size() && info.ModTime().Equal(last.info.ModTime()) {
			return nil
		}
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := statusSnapshotFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, statusSnapshotFile); err != nil {
		return err
	}
	info, _ := os.Lstat(path)
	last.path, last.hash, last.at, last.info = path, hash, now, info
	return nil
}
