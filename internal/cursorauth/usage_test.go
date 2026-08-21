package cursorauth

import (
	"context"
	"io"
	"net/http"
	"testing"
)

func TestFetchPeriodUsageUsesIncludedSpendPercent(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != UsageRPC {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{
			"billingCycleStart":"1000000",
			"billingCycleEnd":"1900000",
			"displayMessage":"You've used 90% of your included usage",
			"planUsage":{"totalSpend":36027,"limit":40000,"remaining":3973,"apiPercentUsed":70.652,"autoPercentUsed":0.35},
			"spendLimitUsage":{"limitType":"user"}
		}`)
	}))
	usage, err := service.FetchPeriodUsage(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchPeriodUsage() error = %v", err)
	}
	if usage.PlanType != "user" || usage.DisplayMessage == "" || len(usage.Windows) != 3 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.Windows[0].Name != "included" || usage.Windows[0].UsedPercent != 90.07 {
		t.Fatalf("included window = %+v", usage.Windows[0])
	}
	if usage.Windows[0].LimitWindowSeconds != 900 || usage.Windows[0].ResetAt != 1900 {
		t.Fatalf("window bounds = %+v", usage.Windows[0])
	}
}

func TestNormalizePeriodUsageRequiresPlanUsage(t *testing.T) {
	t.Parallel()
	if _, err := normalizePeriodUsage(&periodUsagePayload{}); err == nil {
		t.Fatal("empty plan usage must fail")
	}
}

func TestPercentOfRoundsToHeadlineShare(t *testing.T) {
	t.Parallel()
	used, limit := 36027.0, 40000.0
	got := percentOf(&used, &limit)
	if got == nil || *got != 90.07 {
		t.Fatalf("percent = %v", got)
	}
}
