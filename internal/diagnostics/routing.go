package diagnostics

// RoutingDecision records a request-time cooldown choice, never raw sessions,
// credentials or an assertion that a provider's subscription has reset.
type RoutingDecision struct {
	FromProvider    string `json:"from_provider"`
	ToProvider      string `json:"to_provider"`
	Model           string `json:"model"`
	ToModel         string `json:"to_model,omitempty"`
	Action          string `json:"action"`
	Reason          string `json:"reason"`
	ThresholdMS     int64  `json:"threshold_ms"`
	RemainingMS     *int64 `json:"remaining_ms,omitempty"`
	CooldownUntilMS int64  `json:"cooldown_until_ms,omitempty"`
	WaitUnknown     bool   `json:"wait_unknown"`
}

func (d *RoutingDecision) Normalize() *RoutingDecision {
	if d == nil {
		return nil
	}
	n := *d
	if !codePattern.MatchString(n.FromProvider) || !codePattern.MatchString(n.ToProvider) || len(n.Model) > 255 || n.Model == "" || n.ThresholdMS < 0 || n.ThresholdMS > 86400000 {
		return nil
	}
	if len(n.ToModel) > 255 {
		return nil
	}
	for _, r := range n.Model + n.ToModel {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == ':' || r == '/' || r == '-') {
			return nil
		}
	}
	if n.Action != "retain" && n.Action != "switch" {
		return nil
	}
	n.Reason = enum(n.Reason, "within_threshold", "exceeds_threshold", "unknown_wait", "no_eligible_alternative", "affinity_disabled")
	if n.Reason == "unknown" {
		return nil
	}
	if n.RemainingMS != nil {
		v := *n.RemainingMS
		if v < 0 || v > 253402300799999 {
			return nil
		}
		n.RemainingMS = &v
	}
	if n.CooldownUntilMS < 0 || n.CooldownUntilMS > 253402300799999 {
		return nil
	}
	return &n
}
