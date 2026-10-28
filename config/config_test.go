package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func safety(t *testing.T, src string) Safety {
	t.Helper()
	cfg := defaults()
	if _, err := toml.Decode(src, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Safety
}

func TestGuardrails(t *testing.T) {
	cases := map[string][]string{
		``:                           nil, // no guardrails by default
		"[safety]\nsafe_mode = true": {"send", "move", "delete"},
		"[safety]\nsafe_mode = true\nallow_send = true":                    {"move", "delete"},
		"[safety]\nsafe_mode = true\nallow_send = true\nallow_move = true": {"delete"},
		"[safety]\nallow_delete = false":                                   {"delete"},
	}
	for src, want := range cases {
		if got := safety(t, src).Guards(); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: guards = %v, want %v", src, got, want)
		}
	}
}

func TestMaySendTo(t *testing.T) {
	s := safety(t, "[safety]\nsafe_mode = true\nsend_allow = [\"example.com\", \"friend@example.org\"]")
	self := "sam.rivera@example.com"
	for addr, want := range map[string]bool{
		"Sam.Rivera@example.com": true,
		"bob@example.com":       true, // allowed domain
		"bob@evil-example.com":  false,
		"bob@example.com.evil":  false,
		"friend@example.org":      true,
		"other@example.org":       false,
		"":                        false,
	} {
		if got := s.MaySendTo(addr, self); got != want {
			t.Errorf("MaySendTo(%q) = %v, want %v", addr, got, want)
		}
	}
	if !safety(t, "[safety]\nsafe_mode = true\nallow_send = true").MaySendTo("anyone@example.com", self) {
		t.Error("allow_send should allow anyone")
	}
	s = safety(t, "[safety]\nsafe_mode = true")
	if !s.MayMoveTo("ZORP-TEST") || s.MayMoveTo("Archive") {
		t.Error("moves in safe mode")
	}
	if s = safety(t, ""); !s.MaySendTo("anyone@example.com", self) || !s.MayMoveTo("Archive") || !s.DeleteAllowed() {
		t.Error("no guardrails by default")
	}
}

func TestNotifyQuietHours(t *testing.T) {
	at := func(hm string) time.Time { tm, _ := time.Parse("15:04", hm); return tm }
	n := Notify{QuietHours: "22:00-07:00"}
	for hm, want := range map[string]bool{"23:30": true, "06:59": true, "07:00": false, "12:00": false, "22:00": true} {
		if n.Quiet(at(hm)) != want {
			t.Errorf("%s: quiet = %v", hm, !want)
		}
	}
	day := Notify{QuietHours: "12:00-13:00"}
	if !day.Quiet(at("12:30")) || day.Quiet(at("13:00")) {
		t.Error("same-day window")
	}
	if _, _, _, err := (Notify{QuietHours: "10pm-7am"}).QuietWindow(); err == nil {
		t.Error("bad quiet hours accepted")
	}
	if (Notify{}).Quiet(at("03:00")) {
		t.Error("no quiet hours set")
	}
}
