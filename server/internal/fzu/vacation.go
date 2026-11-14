package fzu

import (
	"strings"
	"time"

	"fzucheckin/internal/model"
)

// dateLayout 假期区间的日期格式。写成 "2006-01-02" 而不是用 RFC3339：
// 用户填的是校历上的日期，不该被迫写时分秒。
const dateLayout = "2006-01-02"

// MatchedVacation 判断 today 是否落在某个假期区间内，命中则返回区间名。
//
// 只按"年月日"比较，不做时区换算：调用方传进来的就是北京时间（schedule.NowCN
// 的产物），在这里再换一次时区会让同一个日期在测试与生产里各自漂移。
//
// 坏区间（日期写错、起止颠倒）逐条跳过而不整体失败——一条手写错的假期区间
// 不该让整个签到服务停摆。
func MatchedVacation(cfg model.Config, today time.Time) (string, bool) {
	year, month, day := today.Date()
	// 用 UTC 零点承载"日期"这个纯日历概念：起止日期也按 UTC 解析，两边口径一致，
	// 比较结果只取决于年月日。
	target := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)

	for _, r := range cfg.Vacation.SkipRanges {
		start, ok := parseVacationDate(r.Start)
		if !ok {
			continue
		}
		end, ok := parseVacationDate(r.End)
		if !ok {
			continue
		}
		if start.After(end) {
			continue // 起止写反的区间永不命中（与 Python 的 start <= d <= end 等价）
		}
		if target.Before(start) || target.After(end) {
			continue
		}
		name := strings.TrimSpace(r.Name)
		if name == "" {
			// 没起名就用区间本身当提示，别让日志里出现一个空字符串。
			name = start.Format(dateLayout) + " ~ " + end.Format(dateLayout)
		}
		return name, true
	}
	return "", false
}

// parseVacationDate 解析 "2006-01-02"，失败返回 ok=false。
func parseVacationDate(text string) (time.Time, bool) {
	t, err := time.ParseInLocation(dateLayout, strings.TrimSpace(text), time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
