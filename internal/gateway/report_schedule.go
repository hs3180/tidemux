package gateway

import (
	"errors"
	"strings"
	"time"
)

// ReportSchedule describes the optional local-time daily report notification.
// An empty value disables scheduled notifications.
type ReportSchedule struct {
	Time    string `json:"time,omitempty"`
	Channel string `json:"channel,omitempty"`
}

func (s ReportSchedule) Validate() error {
	if s == (ReportSchedule{}) {
		return nil
	}
	if strings.TrimSpace(s.Time) == "" {
		return errors.New("report_schedule.time is required")
	}
	if _, err := NormalizeReportScheduleTime(s.Time); err != nil {
		return err
	}
	if s.Channel != "" && s.Channel != "macos" && s.Channel != "smtp" {
		return errors.New("report_schedule.channel must be macos or smtp")
	}
	return nil
}

// NormalizeReportScheduleTime validates and canonicalizes a local 24-hour time.
func NormalizeReportScheduleTime(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) != 5 || value[2] != ':' || !isDecimal(value[0]) || !isDecimal(value[1]) || !isDecimal(value[3]) || !isDecimal(value[4]) {
		return "", errors.New("report notification time must use HH:MM")
	}
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return "", errors.New("report notification time must use HH:MM")
	}
	return parsed.Format("15:04"), nil
}

func isDecimal(value byte) bool { return value >= '0' && value <= '9' }

func (s ReportSchedule) HourMinute() (int, int, error) {
	normalized, err := NormalizeReportScheduleTime(s.Time)
	if err != nil {
		return 0, 0, err
	}
	parsed, err := time.Parse("15:04", normalized)
	if err != nil {
		return 0, 0, err
	}
	return parsed.Hour(), parsed.Minute(), nil
}

func (s ReportSchedule) EffectiveChannel() string {
	if s.Channel == "" {
		return "macos"
	}
	return s.Channel
}
