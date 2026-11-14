package fzu

import (
	"testing"
	"time"

	"fzucheckin/internal/model"
)

func vacationConfig(ranges ...model.VacationRange) model.Config {
	return model.Config{Vacation: model.VacationConfig{SkipRanges: ranges}}
}

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 21, 30, 0, 0, cstZone)
}

func TestMatchedVacationHitsNamedRange(t *testing.T) {
	cfg := vacationConfig(
		model.VacationRange{Name: "寒假", Start: "2027-01-23", End: "2027-02-21"},
		model.VacationRange{Name: "暑假", Start: "2027-07-03", End: "2027-08-29"},
	)
	cases := []struct {
		name  string
		today time.Time
		want  bool
		wantN string
	}{
		{"区间中间", date(2027, 1, 30), true, "寒假"},
		{"起点当天（含）", date(2027, 1, 23), true, "寒假"},
		{"终点当天（含）", date(2027, 2, 21), true, "寒假"},
		{"区间前一天", date(2027, 1, 22), false, ""},
		{"区间后一天", date(2027, 2, 22), false, ""},
		{"另一个区间", date(2027, 7, 4), true, "暑假"},
		{"学期中", date(2026, 11, 14), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, hit := MatchedVacation(cfg, tc.today)
			if hit != tc.want {
				t.Fatalf("命中 = %v，期望 %v", hit, tc.want)
			}
			if hit && name != tc.wantN {
				t.Errorf("区间名 = %q，期望 %q", name, tc.wantN)
			}
		})
	}
}

func TestMatchedVacationUnnamedRangeFallsBackToText(t *testing.T) {
	cfg := vacationConfig(model.VacationRange{Start: "2027-01-23", End: "2027-02-21"})
	name, hit := MatchedVacation(cfg, date(2027, 2, 1))
	if !hit {
		t.Fatal("应命中")
	}
	if name != "2027-01-23 ~ 2027-02-21" {
		t.Errorf("无名字的区间应回退成日期文本，实际 %q", name)
	}
}

func TestMatchedVacationSkipsBrokenRanges(t *testing.T) {
	cfg := vacationConfig(
		model.VacationRange{Name: "坏日期", Start: "2027-1-3", End: "2027-02-21"},
		model.VacationRange{Name: "空", Start: "", End: "2027-02-21"},
		model.VacationRange{Name: "起止颠倒", Start: "2027-03-01", End: "2027-02-01"},
		model.VacationRange{Name: "好的", Start: " 2027-02-01 ", End: "2027-02-10 "},
	)
	name, hit := MatchedVacation(cfg, date(2027, 2, 5))
	if !hit {
		t.Fatal("坏区间应被跳过，好区间仍要命中")
	}
	if name != "好的" {
		t.Errorf("区间名 = %q", name)
	}
	// 起止颠倒的区间不该命中它覆盖的日期范围。
	if _, hit := MatchedVacation(cfg, date(2027, 2, 20)); hit {
		t.Error("起止颠倒的区间永不命中")
	}
}

func TestMatchedVacationEmptyConfig(t *testing.T) {
	if name, hit := MatchedVacation(model.Config{}, date(2027, 2, 1)); hit || name != "" {
		t.Errorf("没配假期时不该命中: %q %v", name, hit)
	}
}

func TestMatchedVacationIgnoresClockTime(t *testing.T) {
	// 只比年月日：同一天 00:01 与 23:59 结论必须一致。
	cfg := vacationConfig(model.VacationRange{Name: "寒假", Start: "2027-01-23", End: "2027-02-21"})
	for _, hour := range []int{0, 12, 23} {
		day := time.Date(2027, 1, 23, hour, 59, 0, 0, cstZone)
		if _, hit := MatchedVacation(cfg, day); !hit {
			t.Errorf("hour=%d 时应命中", hour)
		}
	}
}
