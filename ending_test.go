package main

import (
	"strings"
	"testing"
	"time"
)

func TestDayRangeTomorrowMexico(t *testing.T) {
	loc, _ := time.LoadLocation("America/Mexico_City")
	// 26 sep 2026 22:00 hora CDMX = 27 sep 04:00 UTC
	now := time.Date(2026, 9, 27, 4, 0, 0, 0, time.UTC)
	s, e, err := dayRange("", loc, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.UTC().Format(time.RFC3339); got != "2026-09-27T06:00:00Z" {
		t.Errorf("start=%s", got)
	}
	if got := e.UTC().Format(time.RFC3339); got != "2026-09-28T06:00:00Z" {
		t.Errorf("end=%s", got)
	}
	// 3 pm CDMX mañana cae dentro
	three := time.Date(2026, 9, 27, 15, 0, 0, 0, loc)
	if three.Before(s) || !three.Before(e) {
		t.Error("3pm should be in range")
	}
}

func TestSafeFilename(t *testing.T) {
	got := safeFilename(`Vintage: "Kimono" / Silk <NEW>?`, "123456", ".jpg")
	if got != "Vintage Kimono Silk NEW - 123456.jpg" {
		t.Errorf("got %q", got)
	}
	long := safeFilename(strings.Repeat("あ", 300), "9", ".png")
	if len([]rune(long)) > 100+len(" - 9.png") {
		t.Errorf("too long: %d", len([]rune(long)))
	}
	if safeFilename("", "", ".jpg") != "sin-titulo.jpg" {
		t.Error("empty title")
	}
}

func TestLargeImage(t *testing.T) {
	if got := largeImageURL("https://i.ebayimg.com/images/g/abc/s-l225.jpg"); got != "https://i.ebayimg.com/images/g/abc/s-l1600.jpg" {
		t.Errorf("got %s", got)
	}
}
