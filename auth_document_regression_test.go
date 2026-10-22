package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthSwitchInvalidDocumentsFailClosed(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `{"requireAuth":null}`, `[]`, `false`,
		`{"requireAuth":"false"}`, `{"requireAuth":0}`, `{"requireAuth":`,
		`{"requireAuth":false,"updatedAt":"invalid"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			isolatePolicies(t)
			resetStatsState(t)
			dir := resetKeyWorld(t)
			path := filepath.Join(dir, authSwitchName)
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := loadAuthSwitch(dir); err != nil {
				t.Fatal(err)
			}
			if !authSwitchEnabled() {
				t.Error("invalid auth switch must require authentication")
			}
			w := httptest.NewRecorder()
			h := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
			if w.Code != http.StatusUnauthorized {
				t.Errorf("invalid auth switch silently opened API: HTTP %d", w.Code)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != raw {
				t.Fatalf("loading invalid auth switch changed its file: %q, %v", got, err)
			}
		})
	}
}

func TestAuthSwitchValidDocumentsAndFirstStart(t *testing.T) {
	isolatePolicies(t)
	dir := resetKeyWorld(t)
	if err := loadAuthSwitch(dir); err != nil {
		t.Fatal(err)
	}
	if authRequired() {
		t.Fatal("first start without an auth switch or keys must remain open")
	}
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"requireAuth":true}`, true},
		{`{"requireAuth":false}`, false},
		{`{"requireAuth":true,"updatedAt":123}`, true},
		{`{"requireAuth":false,"updatedAt":123,"futureField":"ignored"}`, false},
	} {
		if err := os.WriteFile(filepath.Join(dir, authSwitchName), []byte(tc.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := loadAuthSwitch(dir); err != nil {
			t.Fatal(err)
		}
		if got := authRequired(); got != tc.want {
			t.Fatalf("loadAuthSwitch(%s): authRequired() = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestKeyStoreInvalidDocumentsPreserveMemoryAndFile(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `[]`, `false`, `"store"`,
		`{"keys":[]}`, `{"version":null,"keys":[]}`, `{"version":0,"keys":[]}`,
		`{"version":999,"keys":[]}`, `{"version":1.5,"keys":[]}`,
		`{"version":1,"keys":{}}`, `{"version":`,
	} {
		t.Run(raw, func(t *testing.T) {
			dir := resetKeyWorld(t)
			_, k := mustCreateKey(t, keyCreateOptions{Name: "existing", QuotaTotal: 10})
			setKeyUsageForTest(k, apiKeyUsage{Total: 3, Lifetime: 3})
			apiKeys.mu.RLock()
			wantIDs, wantPrefixes := len(apiKeys.byID), len(apiKeys.byPrefix)
			apiKeys.mu.RUnlock()
			path := filepath.Join(dir, "apikeys.json")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := keyStoreLoad(); err == nil {
				t.Error("invalid persisted key store was accepted")
			}
			apiKeys.mu.RLock()
			unchanged := len(apiKeys.byID) == wantIDs && len(apiKeys.byPrefix) == wantPrefixes &&
				apiKeys.byID[k.ID] == k && apiKeys.byPrefix[k.Prefix] == k && apiKeys.requireAuth
			apiKeys.mu.RUnlock()
			if !unchanged {
				t.Error("invalid key store cleared or replaced the in-memory keys or auth requirement")
			}
			k.mu.Lock()
			usage := k.usage
			k.mu.Unlock()
			if usage.Total != 3 || usage.Lifetime != 3 {
				t.Errorf("invalid key store changed existing usage: %+v", usage)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != raw {
				t.Fatalf("loading invalid key store changed its file: %q, %v", got, err)
			}
		})
	}
}

func TestKeyStoreValidV1DocumentsAndFirstStart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		raw         string
		wantKey     bool
		wantRequire bool
	}{
		{name: "first start"},
		{name: "empty v1", raw: `{"version":1,"keys":[]}`},
		{name: "empty v1 requiring auth", raw: `{"version":1,"keys":[],"requireAuth":true}`, wantRequire: true},
		{name: "historical v1 without requireAuth", raw: `{"version":1,"keys":[{"id":"test","prefix":"wbk_test","hash":"synthetic-hash","enabled":true}]}`, wantKey: true, wantRequire: true},
		{name: "v1 managed key requires auth", raw: `{"version":1,"keys":[{"id":"test","prefix":"wbk_test","hash":"synthetic-hash","enabled":true}],"requireAuth":false}`, wantKey: true, wantRequire: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := resetKeyWorld(t)
			if tc.raw != "" {
				if err := os.WriteFile(filepath.Join(dir, "apikeys.json"), []byte(tc.raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := keyStoreLoad(); err != nil {
				t.Fatalf("valid key store rejected: %v", err)
			}
			apiKeys.mu.RLock()
			gotKey, gotRequire := apiKeys.byID["test"] != nil, apiKeys.requireAuth
			apiKeys.mu.RUnlock()
			if gotKey != tc.wantKey || gotRequire != tc.wantRequire {
				t.Fatalf("key = %v, requireAuth = %v; want %v, %v", gotKey, gotRequire, tc.wantKey, tc.wantRequire)
			}
		})
	}
}
