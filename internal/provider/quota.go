package provider

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/redact"
	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/transport"
	"github.com/tidwall/gjson"
)

const quotaDisplayName = "GitHub Copilot"

// Quota payloads mirror the quota.* RPC contract of CLIProxyAPI hosts with
// plugin quota support; the pinned v7.2.118 SDK does not define these types.
type QuotaFetchRequest struct {
	StorageJSON    []byte         `json:"storage_json,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	HostCallbackID string         `json:"host_callback_id,omitempty"`
}

type QuotaDescribeResponse struct {
	SupportedProviders []string `json:"supported_providers"`
	DisplayName        string   `json:"display_name"`
	SupportsReset      bool     `json:"supports_reset"`
}

type QuotaFetchResponse struct {
	Subscription *QuotaSubscription `json:"subscription,omitempty"`
	Groups       []QuotaGroup       `json:"groups,omitempty"`
}

type QuotaSubscription struct {
	Plan     string `json:"plan,omitempty"`
	TierName string `json:"tierName,omitempty"`
	TierID   string `json:"tierId,omitempty"`
}

type QuotaGroup struct {
	DisplayName string        `json:"displayName"`
	Buckets     []QuotaBucket `json:"buckets"`
}

type QuotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

type QuotaResetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

var copilotQuotaKinds = []struct {
	key   string
	label string
}{
	{key: "premium_interactions", label: "Premium requests"},
	{key: "chat", label: "Chat"},
	{key: "completions", label: "Code completions"},
}

func (s *Service) DescribeQuota() QuotaDescribeResponse {
	return QuotaDescribeResponse{SupportedProviders: []string{providerID}, DisplayName: quotaDisplayName}
}

func (s *Service) ResetQuota() QuotaResetResponse {
	return QuotaResetResponse{Message: "GitHub resets Copilot quotas on its own schedule"}
}

func (s *Service) FetchQuota(ctx context.Context, req QuotaFetchRequest) (QuotaFetchResponse, error) {
	storage, errStorage := quotaStorage(req)
	if errStorage != nil {
		return QuotaFetchResponse{}, errStorage
	}
	cfg := s.Config()
	resp, errDo := s.host.Do(ctx, req.HostCallbackID, transport.Request{
		Method: http.MethodGet,
		URL:    cfg.GitHubAPIURL + "/copilot_internal/user",
		Headers: http.Header{
			"Accept":                []string{"application/json"},
			"Authorization":         []string{"token " + storage.GitHubAccessToken},
			"Editor-Plugin-Version": []string{copilotPluginVersion},
			"Editor-Version":        []string{copilotEditorVersion},
			"User-Agent":            []string{copilotUserAgent},
			"X-GitHub-Api-Version":  []string{copilotAPIVersion},
		},
	})
	if errDo != nil {
		return QuotaFetchResponse{}, fmt.Errorf("fetch Copilot usage: %w", errDo)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return QuotaFetchResponse{}, upstreamStatusError(resp.StatusCode, redact.ErrorBody(resp.Body, storage.GitHubAccessToken))
	}
	return quotaFromCopilotUser(resp.Body)
}

// quotaStorage falls back to metadata because hosts fill StorageJSON only from
// a credential file on disk.
func quotaStorage(req QuotaFetchRequest) (authStorage, error) {
	storage, errStorage := parseStorage(req.StorageJSON)
	if errStorage == nil || len(req.Metadata) == 0 {
		return storage, errStorage
	}
	raw, errMarshal := json.Marshal(req.Metadata)
	if errMarshal != nil {
		return storage, errStorage
	}
	if fallback, errFallback := parseStorage(raw); errFallback == nil {
		return fallback, nil
	}
	return storage, errStorage
}

func quotaFromCopilotUser(body []byte) (QuotaFetchResponse, error) {
	if !gjson.ValidBytes(body) {
		return QuotaFetchResponse{}, fmt.Errorf("decode Copilot usage response: invalid JSON")
	}
	user := gjson.ParseBytes(body)
	var out QuotaFetchResponse
	plan := strings.TrimSpace(user.Get("copilot_plan").String())
	sku := strings.TrimSpace(user.Get("access_type_sku").String())
	if plan != "" || sku != "" {
		out.Subscription = &QuotaSubscription{Plan: plan, TierName: copilotPlanName(plan, sku), TierID: sku}
	}
	limitedReset := resetTime(user.Get("limited_user_reset_date"))
	accountReset := cmp.Or(resetTime(user.Get("quota_reset_date_utc")), resetTime(user.Get("quota_reset_date")), limitedReset)
	var buckets []QuotaBucket
	for _, kind := range copilotQuotaKinds {
		bucket, ok := snapshotBucket(kind.label, user.Get("quota_snapshots."+kind.key), accountReset)
		if !ok {
			bucket, ok = limitedBucket(kind.label, user.Get("limited_user_quotas."+kind.key), user.Get("monthly_quotas."+kind.key), cmp.Or(limitedReset, accountReset))
		}
		if ok {
			buckets = append(buckets, bucket)
		}
	}
	if len(buckets) > 0 {
		out.Groups = []QuotaGroup{{DisplayName: quotaDisplayName, Buckets: buckets}}
	}
	return out, nil
}

func snapshotBucket(label string, snapshot gjson.Result, accountReset string) (QuotaBucket, bool) {
	if !snapshot.IsObject() {
		return QuotaBucket{}, false
	}
	bucket := QuotaBucket{
		Window:    "monthly",
		ResetTime: cmp.Or(resetTime(snapshot.Get("quota_reset_at")), accountReset),
	}
	entitlement, hasEntitlement := quotaNumber(snapshot.Get("entitlement"))
	// Copilot quota snapshots use an entitlement of -1 for unlimited quotas.
	if snapshot.Get("unlimited").Bool() || (hasEntitlement && entitlement < 0) {
		bucket.RemainingFraction = 1
		bucket.Description = label + " (unlimited)"
		return bucket, true
	}
	if hasEntitlement && entitlement == 0 {
		// The plan has no allocation for this quota, such as premium requests on Copilot Free.
		return QuotaBucket{}, false
	}
	remaining, hasRemaining := quotaNumber(snapshot.Get("quota_remaining"))
	if !hasRemaining {
		remaining, hasRemaining = quotaNumber(snapshot.Get("remaining"))
	}
	percent, hasPercent := quotaNumber(snapshot.Get("percent_remaining"))
	switch {
	case hasPercent:
		bucket.RemainingFraction = percent / 100
	case hasRemaining && hasEntitlement:
		bucket.RemainingFraction = remaining / entitlement
	default:
		return QuotaBucket{}, false
	}
	bucket.RemainingFraction = max(0, min(1, bucket.RemainingFraction))
	bucket.Description = label
	if hasRemaining && hasEntitlement {
		bucket.Description = fmt.Sprintf("%s (%s/%s)", label, formatQuotaCount(remaining), formatQuotaCount(entitlement))
	}
	return bucket, true
}

func limitedBucket(label string, remaining, monthly gjson.Result, reset string) (QuotaBucket, bool) {
	left, hasLeft := quotaNumber(remaining)
	total, hasTotal := quotaNumber(monthly)
	if !hasLeft || !hasTotal || total <= 0 {
		return QuotaBucket{}, false
	}
	return QuotaBucket{
		Window:            "monthly",
		RemainingFraction: max(0, min(1, left/total)),
		ResetTime:         reset,
		Description:       fmt.Sprintf("%s (%s/%s)", label, formatQuotaCount(left), formatQuotaCount(total)),
	}, true
}

func quotaNumber(value gjson.Result) (float64, bool) {
	var number float64
	switch value.Type {
	case gjson.Number:
		number = value.Num
	case gjson.String:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(value.Str), 64)
		if errParse != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func resetTime(value gjson.Result) string {
	switch value.Type {
	case gjson.Number:
		switch {
		case value.Num > 1e11:
			// Values this large are millisecond timestamps.
			return time.UnixMilli(int64(value.Num)).UTC().Format(time.RFC3339)
		case value.Num > 0:
			return time.Unix(int64(value.Num), 0).UTC().Format(time.RFC3339)
		}
	case gjson.String:
		text := strings.TrimSpace(value.Str)
		for _, layout := range []string{time.RFC3339, time.DateOnly} {
			if parsed, errParse := time.Parse(layout, text); errParse == nil {
				return parsed.UTC().Format(time.RFC3339)
			}
		}
		return text
	}
	return ""
}

func formatQuotaCount(value float64) string {
	return strconv.FormatFloat(math.Round(max(0, value)*10)/10, 'f', -1, 64)
}

func copilotPlanName(plan, sku string) string {
	switch sku {
	case "free_limited_copilot":
		return "Copilot Free"
	case "free_educational_quota":
		return "Copilot Student"
	}
	switch plan {
	case "individual_edu":
		return "Copilot Student"
	case "individual":
		return "Copilot Pro"
	case "individual_pro":
		return "Copilot Pro+"
	case "individual_max":
		return "Copilot Max"
	case "business":
		return "Copilot Business"
	case "enterprise":
		return "Copilot Enterprise"
	}
	return ""
}
