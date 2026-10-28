package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestDiagnosticsLinksAuditTraceWithoutBodyOrKey(t *testing.T) {
	old := phaseMetrics
	phaseMetrics = &performanceStore{}
	defer func() { phaseMetrics = old }()
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("private-body-marker"))
	r.Header.Set("X-Trace-ID", "ccp-synthetic-linked-request")
	r.Header.Set("Authorization", "Bearer synthetic-secret-marker")
	h := requestAuditMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		performanceFor(r).observe(phaseRead, 2*time.Millisecond, false)
		w.WriteHeader(200)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	d, ok := phaseMetrics.requestDiagnostic(w.Header().Get("X-Trace-ID"), time.Now())
	if !ok || d.TraceID != "ccp-synthetic-linked-request" || d.Phases["request_read"] != 2 {
		t.Fatalf("audit trace not linked: %+v", d)
	}
	encoded, _ := json.Marshal(d)
	if strings.Contains(string(encoded), "private-body-marker") || strings.Contains(string(encoded), "synthetic-secret-marker") {
		t.Fatal("request data entered diagnostics")
	}
}

func TestRequestDiagnosticsBoundedPrivateAndExpires(t *testing.T) {
	s := &performanceStore{}
	now := time.Now()
	for i := 0; i < requestDiagnosticCapacity+1; i++ {
		p := &requestPerformance{traceID: fmt.Sprintf("synthetic-%d", i), protocol: 2, start: now.Add(-time.Second), streamSeen: true, streamComplete: true}
		p.observe(phaseAdmission, 10*time.Millisecond, false)
		p.finish(s, 200, false, now)
		p.finish(s, 500, true, now) // A finalization is recorded exactly once.
	}
	if s.recent.count != requestDiagnosticCapacity {
		t.Fatal("unbounded recent request storage")
	}
	if _, ok := s.requestDiagnostic("synthetic-0", now); ok {
		t.Fatal("oldest record was not evicted")
	}
	d, ok := s.requestDiagnostic("synthetic-128", now)
	if !ok || d.Status != 200 || d.Phases["admission_wait"] != 10 || d.Counters.Requests != 1 {
		t.Fatalf("bad diagnostic: %+v", d)
	}
	d.Phases["admission_wait"] = 999
	copy, _ := s.requestDiagnostic("synthetic-128", now)
	if copy.Phases["admission_wait"] != 10 {
		t.Fatal("mutable diagnostic alias")
	}
	if _, ok := s.requestDiagnostic("synthetic-128", now.Add(requestDiagnosticTTL+time.Millisecond)); ok {
		t.Fatal("expired diagnostic exposed")
	}
	encoded, _ := json.Marshal(copy)
	for _, name := range []string{"model", "account", "authorization", "apiKey", "prompt"} {
		if strings.Contains(string(encoded), name) {
			t.Fatalf("sensitive diagnostic field: %s", name)
		}
	}
}

func TestRequestDiagnosticsValidationAndAdminAuthentication(t *testing.T) {
	for _, value := range []string{"", strings.Repeat("a", 65), "line\nbreak", "private/prompt", "secret?x=1", "中文"} {
		if validDiagnosticTrace(value) {
			t.Fatalf("accepted unsafe trace: %q", value)
		}
	}
	isolatePolicies(t)
	if err := applyGatewayConfig(gatewayFileConfig{AdminKey: "synthetic-diagnostic-admin"}); err != nil {
		t.Fatal(err)
	}
	h := newWebUIHandler()
	if w := adminRequest(h, "GET", "/admin/api/performance?trace=synthetic", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("diagnostics exposed: %d", w.Code)
	}
	if w := adminRequest(h, "GET", "/admin/api/performance?trace=unsafe%2Ftrace", "synthetic-diagnostic-admin", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad trace accepted: %d", w.Code)
	}
	if w := adminRequest(h, "GET", "/admin/api/performance?trace=missing", "synthetic-diagnostic-admin", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing trace status: %d", w.Code)
	}
}
