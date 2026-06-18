package sync

import "testing"

func TestIsQuietHoursDisabledOnBadInput(t *testing.T) {
	if IsQuietHours("", "07:00") {
		t.Fatal("empty start should disable quiet hours")
	}
	if IsQuietHours("22:00", "") {
		t.Fatal("empty end should disable quiet hours")
	}
	if IsQuietHours("nope", "07:00") {
		t.Fatal("unparseable time should disable quiet hours")
	}
}

func TestParseHHMM(t *testing.T) {
	if h, m, ok := parseHHMM("22:05"); !ok || h != 22 || m != 5 {
		t.Fatalf("parseHHMM(22:05) = %d,%d,%v", h, m, ok)
	}
	if _, _, ok := parseHHMM("24:00"); ok {
		t.Fatal("hour 24 should be rejected")
	}
	if _, _, ok := parseHHMM("12:60"); ok {
		t.Fatal("minute 60 should be rejected")
	}
	if _, _, ok := parseHHMM("1230"); ok {
		t.Fatal("missing colon should be rejected")
	}
}
