package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/stevejkang/infinitic/internal/quota"
)

// liveUsage is trimmed from a real response (Max account, 2026-10-19).
const liveUsage = `{
 "five_hour": {"utilization": 9.0, "resets_at": "2026-10-19T05:00:00.211144+00:00", "limit_dollars": null},
 "seven_day": {"utilization": 19.0, "resets_at": "2026-10-20T11:00:00.211163+00:00"},
 "seven_day_opus": null, "seven_day_sonnet": null, "tangelo": null,
 "juniper_tide": {"eligible": false},
 "limits": [
  {"kind": "session", "group": "session", "percent": 9, "resets_at": "2026-10-19T05:00:00.211144+00:00", "scope": null, "is_active": false},
  {"kind": "weekly_all", "group": "weekly", "percent": 19, "resets_at": "2026-10-20T11:00:00.211163+00:00", "scope": null, "is_active": true},
  {"kind": "weekly_scoped", "group": "weekly", "percent": 0, "resets_at": "2026-10-20T11:00:00+00:00",
   "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": false}
 ],
 "seven_day_breakdown": {"rows": []}
}`

func parse(t *testing.T, body string) quota.Usage {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	u, err := parseUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func labels(u quota.Usage) []string {
	var out []string
	for _, w := range u.Others() {
		out = append(out, w.Label)
	}
	return out
}

func TestUsageReadsTheLiveShape(t *testing.T) {
	u := parse(t, liveUsage)
	want := []struct {
		label   string
		percent float64
	}{{"5h", 9}, {"7d", 19}, {"Fable", 0}}
	if len(u.Windows) != len(want) {
		t.Fatalf("windows %+v", u.Windows)
	}
	for i, w := range want {
		got := u.Windows[i]
		if got.Label != w.label || got.Percent != w.percent || got.ResetsAt == nil {
			t.Errorf("window %d: %+v, want %+v", i, got, w)
		}
	}
}

func TestUsageTakesNewScopedLimitsWithoutCodeChanges(t *testing.T) {
	u := parse(t, `{"five_hour":{"utilization":1},"seven_day":{"utilization":2},"limits":[
	 {"kind":"weekly_scoped","percent":61,"scope":{"model":{"display_name":"Opus"}}},
	 {"kind":"daily_surface","percent":140},
	 {"kind":"weekly_scoped","percent":20,"scope":{"model":{"display_name":"All models"}}},
	 {"kind":"weekly_scoped","percent":99,"scope":{"model":{"display_name":"opus"}}},
	 {"kind":7,"percent":5},
	 {"kind":"weekly_scoped","scope":{"model":{"display_name":"Haiku"}}}
	]}`)
	got := labels(u)
	if len(got) != 2 || got[0] != "Opus" || got[1] != "daily_surface" {
		t.Fatalf("scoped %v", got)
	}
	if u.Windows[2].Percent != 61 || u.Windows[3].Percent != 100 {
		t.Fatalf("percents %+v", u.Windows)
	}
}

func TestUsageSurvivesALimitsFieldThatIsNotAnArray(t *testing.T) {
	for _, limits := range []string{`{"weekly_scoped":[]}`, `"x"`, `7`, `null`} {
		u := parse(t, `{"five_hour":{"utilization":41},"seven_day":{"utilization":20},"limits":`+limits+`}`)
		if u.Window(quota.SessionLabel).Percent != 41 || len(u.Windows) != 2 {
			t.Errorf("limits=%s: %+v", limits, u)
		}
	}
}

func TestUsageFallsBackToLegacyKeysAndToLimitsForTheMainWindows(t *testing.T) {
	legacy := parse(t, `{"five_hour":{"utilization":1},"seven_day":{"utilization":2},"seven_day_opus":{"utilization":60},"seven_day_sonnet":null}`)
	if got := labels(legacy); len(got) != 1 || got[0] != "Opus" || legacy.Windows[2].Percent != 60 {
		t.Fatalf("legacy windows %+v", legacy.Windows)
	}

	onlyLimits := parse(t, `{"limits":[{"kind":"session","percent":30},{"kind":"weekly_all","percent":40}]}`)
	if onlyLimits.Window(quota.SessionLabel).Percent != 30 || onlyLimits.Window(quota.WeeklyLabel).Percent != 40 {
		t.Fatalf("windows from limits %+v", onlyLimits)
	}

	for _, body := range []string{`{"limits":[]}`, `{"five_hour":{"utilization":3},"seven_day":null}`} {
		var raw map[string]json.RawMessage
		_ = json.Unmarshal([]byte(body), &raw)
		if _, err := parseUsage(raw); err == nil {
			t.Fatalf("%s: a response with no 7-day window must fail", body)
		}
	}
}

func TestUsageWithoutASessionLimit(t *testing.T) {
	u := parse(t, `{"five_hour":null,"seven_day":{"utilization":12,"resets_at":"2026-11-05T10:00:00+00:00"},"limits":[
	 {"kind":"weekly_all","group":"weekly","percent":12,"resets_at":"2026-11-05T10:00:00+00:00","scope":null,"is_active":true}
	]}`)
	if u.Window(quota.SessionLabel) != nil || u.Window(quota.WeeklyLabel).Percent != 12 || len(u.Windows) != 1 {
		t.Fatalf("windows %+v", u.Windows)
	}
}

func TestUsageKeepsASessionWindowThatStaysAtZero(t *testing.T) {
	u := parse(t, `{"five_hour":{"utilization":0,"resets_at":"2026-11-03T05:00:00+00:00"},"seven_day":{"utilization":30}}`)
	if w := u.Window(quota.SessionLabel); w == nil || w.Percent != 0 || w.ResetsAt == nil {
		t.Fatalf("windows %+v", u.Windows)
	}
}

func TestUsagePrefersLimitsOverTheLegacyKeys(t *testing.T) {
	u := parse(t, `{"five_hour":{"utilization":1},"seven_day":{"utilization":2},"limits":[
	 {"kind":"session","percent":30,"is_active":false},{"kind":"weekly_all","percent":40,"is_active":false}]}`)
	if u.Window(quota.SessionLabel).Percent != 30 || u.Window(quota.WeeklyLabel).Percent != 40 {
		t.Fatalf("windows %+v", u.Windows)
	}
}
