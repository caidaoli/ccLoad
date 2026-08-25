package app

import (
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestManagementCheckinDueUsesServerLocalDateAndTime(t *testing.T) {
	loc := time.FixedZone("server", 8*60*60)
	oldLocal := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = oldLocal })
	base := time.Date(2026, 8, 26, 9, 0, 0, 0, loc)
	newEnvelope := func(day string) *model.ChannelManagementEnvelope {
		return &model.ChannelManagementEnvelope{
			Profile:  model.ChannelManagementProfileNewAPI,
			Settings: model.ChannelManagementSettings{DailyCheckinEnabled: true, DailyCheckinTime: "09:00"},
			State:    model.ChannelManagementState{LastScheduledDay: day},
		}
	}
	for name, tc := range map[string]struct {
		now  time.Time
		day  string
		want bool
	}{
		"before time":          {now: base.Add(-time.Minute), want: false},
		"at time":              {now: base, want: true},
		"same local day":       {now: base.Add(time.Hour), day: "2026-08-26", want: false},
		"previous day can run": {now: base, day: "2026-08-25", want: true},
		"local date matters":   {now: time.Date(2026, 8, 26, 1, 0, 0, 0, loc), want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isManagementCheckinDue(newEnvelope(tc.day), tc.now); got != tc.want {
				t.Fatalf("isManagementCheckinDue()=%v, want %v at %s", got, tc.want, tc.now)
			}
		})
	}
}

func TestManagementCheckinDueSkipsUnsupportedOrDisabledProfiles(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.FixedZone("server", 8*60*60))
	cases := []model.ChannelManagementEnvelope{
		{Profile: model.ChannelManagementProfileSub2API, Settings: model.ChannelManagementSettings{DailyCheckinEnabled: true, DailyCheckinTime: "09:00"}},
		{Profile: model.ChannelManagementProfileNewAPI, Settings: model.ChannelManagementSettings{DailyCheckinTime: "09:00"}},
		{Profile: model.ChannelManagementProfileNewAPI, Settings: model.ChannelManagementSettings{DailyCheckinEnabled: true, DailyCheckinTime: "invalid"}},
	}
	for _, envelope := range cases {
		if isManagementCheckinDue(&envelope, now) {
			t.Fatalf("unsupported/disabled/invalid envelope was due: %#v", envelope)
		}
	}
}
