package main

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestStatusSnapshotPendingCheckinKeepsLiveEligibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		modify   func(*Account)
		eligible bool
	}{
		{"active", func(*Account) {}, true},
		{"missing-token", func(a *Account) { a.Auth.Auth.AccessToken = "" }, false},
		{"disabled", func(a *Account) { a.Disabled = true }, false},
		{"international", func(a *Account) { a.Auth.Edition = "intl" }, false},
		{"enterprise", func(a *Account) { a.Auth.Account.EnterpriseID = "synthetic-enterprise" }, false},
		{"missing-auth", func(a *Account) { a.Auth = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateAdminFeatures(t)
			s, now, initial := checkinFixture(t)
			calls := 0
			s.perform = func(context.Context, *Account, string) (string, error) { calls++; return "unexpected", nil }
			accountMu.Lock()
			tc.modify(initial.account)
			target := checkinTargetLocked(initial.account)
			accountMu.Unlock()
			if target.eligible != tc.eligible {
				t.Fatal("invalid eligibility fixture")
			}
			// There is no saved plan yet: this is the pending display path that
			// must use live eligibility without publishing an access token.
			want := s.view(target, *now)
			before, err := json.Marshal(s.doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeStatusSnapshotAt(*now); err != nil {
				t.Fatal(err)
			}
			snapshot := loadStatusForTest(t)
			if len(snapshot.Accounts) != 1 {
				t.Fatal("missing snapshot account")
			}
			if got := snapshot.Accounts[0].Checkin; !reflect.DeepEqual(got, want) {
				t.Errorf("pending checkin differs from live eligibility: got=%+v want=%+v", got, want)
			}
			if tc.eligible && (snapshot.Accounts[0].Checkin.Status != "pending" || strings.Contains(snapshot.Accounts[0].Checkin.Message, "账号当前不可用")) {
				t.Error("active pending account was presented as unavailable")
			}
			after, err := json.Marshal(s.doc)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) || calls != 0 {
				t.Error("status display changed or dispatched the checkin schedule")
			}
			raw, err := os.ReadFile(statusSnapshotFile)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "synthetic-checkin-token") {
				t.Error("snapshot exposed an access token")
			}
		})
	}
}

func TestStatusSnapshotCapturedCheckinDoesNotRetainLiveAccount(t *testing.T) {
	isolateAdminFeatures(t)
	s, now, target := checkinFixture(t)
	want := s.view(target, *now)
	captured := captureStatusSnapshotAccounts()[0]
	if captured.Auth.Auth.AccessToken != "" || captured.checkin.account != nil {
		t.Fatal("checkin presentation retained access token or live account")
	}
	if captured.checkin.key != target.key || !captured.checkin.supported || !captured.checkin.eligible {
		t.Fatal("checkin metadata was not captured from the live account")
	}
	accountMu.Lock()
	target.account.Auth.Auth.AccessToken = ""
	target.account.Auth.Account.UID = "changed-checkin-user"
	target.account.Auth.Account.EnterpriseID = "changed-enterprise"
	target.account.Disabled = true
	current := checkinTargetLocked(target.account)
	accountMu.Unlock()
	if current.eligible || current.key == captured.checkin.key {
		t.Fatal("fixture did not change live checkin identity")
	}
	if got := s.view(captured.checkin, *now); !reflect.DeepEqual(got, want) {
		t.Fatalf("captured checkin changed with its source: got=%+v want=%+v", got, want)
	}
}
