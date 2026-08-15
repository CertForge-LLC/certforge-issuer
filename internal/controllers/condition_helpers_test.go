package controllers

import (
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
)

func makeCR() *cmapi.CertificateRequest {
	return &cmapi.CertificateRequest{}
}

// ── setCondition ──────────────────────────────────────────────────────────────

func TestSetCondition_AddsNewCondition(t *testing.T) {
	cr := makeCR()
	setCondition(cr, cmapi.CertificateRequestConditionReady, cmmeta.ConditionFalse, "Pending", "waiting for approval")

	if len(cr.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(cr.Status.Conditions))
	}
	c := cr.Status.Conditions[0]
	if c.Type != cmapi.CertificateRequestConditionReady {
		t.Errorf("Type = %v, want Ready", c.Type)
	}
	if c.Status != cmmeta.ConditionFalse {
		t.Errorf("Status = %v, want False", c.Status)
	}
	if c.Reason != "Pending" {
		t.Errorf("Reason = %q, want Pending", c.Reason)
	}
	if c.Message != "waiting for approval" {
		t.Errorf("Message = %q, want 'waiting for approval'", c.Message)
	}
	if c.LastTransitionTime == nil {
		t.Error("LastTransitionTime must be set for new conditions")
	}
}

func TestSetCondition_UpdatesExistingCondition(t *testing.T) {
	cr := makeCR()
	setCondition(cr, cmapi.CertificateRequestConditionReady, cmmeta.ConditionFalse, "Pending", "first")
	setCondition(cr, cmapi.CertificateRequestConditionReady, cmmeta.ConditionTrue, "Issued", "second")

	if len(cr.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(cr.Status.Conditions))
	}
	c := cr.Status.Conditions[0]
	if c.Status != cmmeta.ConditionTrue {
		t.Errorf("Status = %v, want True after update", c.Status)
	}
	if c.Reason != "Issued" {
		t.Errorf("Reason = %q, want Issued", c.Reason)
	}
	if c.Message != "second" {
		t.Errorf("Message = %q, want second", c.Message)
	}
}

func TestSetCondition_MultipleTypes(t *testing.T) {
	cr := makeCR()
	setCondition(cr, cmapi.CertificateRequestConditionReady, cmmeta.ConditionFalse, "Pending", "")
	setCondition(cr, cmapi.CertificateRequestConditionApproved, cmmeta.ConditionTrue, "Approved", "")

	if len(cr.Status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions for 2 types, got %d", len(cr.Status.Conditions))
	}
}

func TestSetCondition_TransitionTimeOnlyUpdatesOnStatusChange(t *testing.T) {
	cr := makeCR()
	setCondition(cr, cmapi.CertificateRequestConditionReady, cmmeta.ConditionFalse, "Pending", "msg")
	original := *cr.Status.Conditions[0].LastTransitionTime

	// Same status, different reason — LastTransitionTime must not change.
	setCondition(cr, cmapi.CertificateRequestConditionReady, cmmeta.ConditionFalse, "StillPending", "msg2")
	if !cr.Status.Conditions[0].LastTransitionTime.Equal(&original) {
		t.Error("LastTransitionTime changed when Status stayed the same")
	}

	// Sleep briefly so the clock advances.
	time.Sleep(2 * time.Millisecond)

	// Different status — LastTransitionTime must change.
	setCondition(cr, cmapi.CertificateRequestConditionReady, cmmeta.ConditionTrue, "Issued", "done")
	updated := cr.Status.Conditions[0].LastTransitionTime
	if updated.Equal(&original) {
		t.Error("LastTransitionTime did not change when Status changed")
	}
}

// ── isConditionTrue ───────────────────────────────────────────────────────────

func TestIsConditionTrue_AbsentCondition(t *testing.T) {
	cr := makeCR()
	if isConditionTrue(cr, cmapi.CertificateRequestConditionReady) {
		t.Error("expected false for absent condition")
	}
}

func TestIsConditionTrue_FalseCondition(t *testing.T) {
	cr := makeCR()
	setCondition(cr, cmapi.CertificateRequestConditionReady, cmmeta.ConditionFalse, "Pending", "")
	if isConditionTrue(cr, cmapi.CertificateRequestConditionReady) {
		t.Error("expected false for ConditionFalse")
	}
}

func TestIsConditionTrue_TrueCondition(t *testing.T) {
	cr := makeCR()
	setCondition(cr, cmapi.CertificateRequestConditionReady, cmmeta.ConditionTrue, "Issued", "")
	if !isConditionTrue(cr, cmapi.CertificateRequestConditionReady) {
		t.Error("expected true for ConditionTrue")
	}
}

func TestIsConditionTrue_OnlyMatchesRequestedType(t *testing.T) {
	cr := makeCR()
	setCondition(cr, cmapi.CertificateRequestConditionApproved, cmmeta.ConditionTrue, "Approved", "")
	if isConditionTrue(cr, cmapi.CertificateRequestConditionDenied) {
		t.Error("Denied should not be true just because Approved is true")
	}
}

// ── setRejectedCondition ──────────────────────────────────────────────────────

func TestSetRejectedCondition_SetsDeniedWhenApprovedAbsent(t *testing.T) {
	cr := makeCR()
	setRejectedCondition(cr, "PolicyViolation", "domain not in DTP")

	if !isConditionTrue(cr, cmapi.CertificateRequestConditionDenied) {
		t.Error("expected Denied=True when Approved condition is absent")
	}
	if isConditionTrue(cr, cmapi.CertificateRequestConditionInvalidRequest) {
		t.Error("should not set InvalidRequest when Approved is not set")
	}
}

func TestSetRejectedCondition_FallsBackToInvalidRequestWhenApproved(t *testing.T) {
	cr := makeCR()
	// Approved=True already set (e.g. by an approver-policy that raced)
	setCondition(cr, cmapi.CertificateRequestConditionApproved, cmmeta.ConditionTrue, "Approved", "")

	setRejectedCondition(cr, "Rejected", "denied by CertForge approver")

	if isConditionTrue(cr, cmapi.CertificateRequestConditionDenied) {
		t.Error("must not set Denied=True when Approved=True already exists (webhook forbids it)")
	}
	if !isConditionTrue(cr, cmapi.CertificateRequestConditionInvalidRequest) {
		t.Error("expected InvalidRequest=True as fallback when Approved=True is set")
	}
}

// ── formatElapsed ─────────────────────────────────────────────────────────────

func TestFormatElapsed(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0m"},
		{30 * time.Second, "0m"}, // less than a minute rounds down to 0m
		{3 * time.Minute, "3m"},
		{59 * time.Minute, "59m"},
		{60 * time.Minute, "1h"},
		{2 * time.Hour, "2h"},
		{90 * time.Minute, "1h 30m"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
	}
	for _, tc := range tests {
		got := formatElapsed(tc.d)
		if got != tc.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
