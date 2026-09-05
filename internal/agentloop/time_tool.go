package agentloop

import (
	"encoding/json"
	"fmt"
	"time"
	_ "time/tzdata" // the image's zoneinfo is not a dependency of current_time
)

func init() { Handlers["current_time"] = currentTime }

// pyISO renders a time as python's datetime.isoformat(): microseconds only
// when non-zero, and a "+HH:MM" offset.
func pyISO(t time.Time) string {
	base := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		base += fmt.Sprintf(".%06d", us)
	}
	return base + t.Format("-07:00")
}

func currentTime(env Env, raw json.RawMessage) string {
	a := decodeArgs(raw)
	tzName := a.str("timezone", "UTC")
	if tzName == "" || tzName == "null" {
		tzName = "UTC"
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return "ERROR: invalid timezone: " + tzName
	}
	now := time.Now
	if env.Now != nil {
		now = env.Now
	}
	utc := now().UTC().Truncate(time.Microsecond)
	local := utc.In(loc)
	_, off := local.Zone()
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	// python: local.dst() is non-zero → is_dst. Go exposes it as the
	// difference between the zone's standard offset and the current one.
	isDST := local.IsDST()
	return pyJSON(pyObject{
		{"timezone", tzName},
		{"date", local.Format("2006-01-02")},
		{"time", local.Format("15:04:05")},
		{"weekday", local.Format("Monday")},
		{"rfc3339", pyISO(local)},
		{"utc", utc.Format("2006-01-02T15:04:05") + microsSuffix(utc) + "Z"},
		{"utc_offset", fmt.Sprintf("%s%02d:%02d", sign, off/3600, (off%3600)/60)},
		{"is_dst", isDST},
		{"unix", utc.Unix()},
	})
}

func microsSuffix(t time.Time) string {
	if us := t.Nanosecond() / 1000; us != 0 {
		return fmt.Sprintf(".%06d", us)
	}
	return ""
}
