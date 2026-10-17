package provider

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/transport"
)

type quotaTestHost struct {
	request  transport.Request
	response transport.Response
}

func (h *quotaTestHost) Do(_ context.Context, _ string, req transport.Request) (transport.Response, error) {
	h.request = req
	return h.response, nil
}

func (*quotaTestHost) OpenStream(context.Context, string, transport.Request) (transport.Stream, error) {
	return transport.Stream{}, errors.New("unexpected stream")
}

func (*quotaTestHost) ReadStream(context.Context, string) (transport.StreamChunk, error) {
	return transport.StreamChunk{}, errors.New("unexpected stream read")
}

func (*quotaTestHost) CloseStream(context.Context, string) error { return nil }

func (*quotaTestHost) Emit(context.Context, string, []byte) error { return nil }

func (*quotaTestHost) CloseOutput(context.Context, string, string) {}

func TestFetchQuotaMapsCopilotUsage(t *testing.T) {
	t.Parallel()

	host := &quotaTestHost{response: transport.Response{StatusCode: http.StatusOK, Body: []byte(`{
		"copilot_plan": "individual",
		"quota_reset_date": "2026-11-01",
		"quota_snapshots": {
			"premium_interactions": {"entitlement": 300, "remaining": 243, "quota_remaining": 243, "percent_remaining": 81, "unlimited": false, "overage_count": 0, "overage_permitted": false},
			"chat": {"entitlement": 0, "remaining": 0, "percent_remaining": 100, "unlimited": true},
			"completions": {"entitlement": 0, "remaining": 0, "percent_remaining": 100, "unlimited": true}
		}
	}`)}}

	resp, err := New(host).FetchQuota(t.Context(), QuotaFetchRequest{
		StorageJSON:    []byte(`{"type":"copilot","github_access_token":"gho_quota_test","github_login":"octocat"}`),
		HostCallbackID: "callback-1",
	})
	if err != nil {
		t.Fatalf("fetch quota: %v", err)
	}
	if got, want := host.request.URL, "https://api.github.com/copilot_internal/user"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got, want := host.request.Headers.Get("Authorization"), "token gho_quota_test"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	wantSubscription := &QuotaSubscription{Plan: "individual", TierName: "Copilot Pro"}
	if !reflect.DeepEqual(resp.Subscription, wantSubscription) {
		t.Errorf("subscription = %+v, want %+v", resp.Subscription, wantSubscription)
	}
	wantGroups := []QuotaGroup{{DisplayName: "GitHub Copilot", Buckets: []QuotaBucket{
		{Window: "monthly", RemainingFraction: 0.81, ResetTime: "2026-11-01T00:00:00Z", Description: "Premium requests (243/300)"},
		{Window: "monthly", RemainingFraction: 1, ResetTime: "2026-11-01T00:00:00Z", Description: "Chat (unlimited)"},
		{Window: "monthly", RemainingFraction: 1, ResetTime: "2026-11-01T00:00:00Z", Description: "Code completions (unlimited)"},
	}}}
	if !reflect.DeepEqual(resp.Groups, wantGroups) {
		t.Errorf("groups = %+v, want %+v", resp.Groups, wantGroups)
	}
}

func TestQuotaFromCopilotUserHandlesLimitedPlans(t *testing.T) {
	t.Parallel()

	resp, err := quotaFromCopilotUser([]byte(`{
		"access_type_sku": "free_limited_copilot",
		"copilot_plan": "individual",
		"limited_user_quotas": {"chat": 40, "completions": 1500},
		"monthly_quotas": {"chat": 50, "completions": 2000},
		"limited_user_reset_date": 1793491200,
		"quota_snapshots": {
			"premium_interactions": {"entitlement": 0, "remaining": 0, "percent_remaining": 0, "unlimited": false}
		}
	}`))
	if err != nil {
		t.Fatalf("map quota: %v", err)
	}
	if resp.Subscription == nil || resp.Subscription.TierName != "Copilot Free" {
		t.Errorf("subscription = %+v, want Copilot Free", resp.Subscription)
	}
	wantGroups := []QuotaGroup{{DisplayName: "GitHub Copilot", Buckets: []QuotaBucket{
		{Window: "monthly", RemainingFraction: 0.8, ResetTime: "2026-11-01T00:00:00Z", Description: "Chat (40/50)"},
		{Window: "monthly", RemainingFraction: 0.75, ResetTime: "2026-11-01T00:00:00Z", Description: "Code completions (1500/2000)"},
	}}}
	if !reflect.DeepEqual(resp.Groups, wantGroups) {
		t.Errorf("groups = %+v, want %+v", resp.Groups, wantGroups)
	}
}

func TestFetchQuotaRedactsUpstreamErrors(t *testing.T) {
	t.Parallel()

	host := &quotaTestHost{response: transport.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       []byte(`{"message":"Bad credentials for gho_metadata_secret"}`),
	}}
	_, err := New(host).FetchQuota(t.Context(), QuotaFetchRequest{
		Metadata: map[string]any{"type": "copilot", "github_access_token": "gho_metadata_secret"},
	})
	statusErr, ok := errors.AsType[*StatusError](err)
	if !ok || statusErr.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("error = %v, want HTTP 401 status error", err)
	}
	if strings.Contains(err.Error(), "gho_metadata_secret") {
		t.Errorf("error leaks the GitHub token: %v", err)
	}
	if got, want := host.request.Headers.Get("Authorization"), "token gho_metadata_secret"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}
