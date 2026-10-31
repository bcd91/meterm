package audit

import "time"

func d(y int, m time.Month, day int) time.Time {
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
}

// supportEnd is when each release stops getting regular security updates
// (for LTS releases the standard window, not the paid extension; for Debian,
// the end of the LTS window). A release missing here is reported as
// "unknown", never as fine.
var supportEnd = map[string]time.Time{
	"ubuntu:14.04": d(2019, time.April, 25),
	"ubuntu:16.04": d(2021, time.April, 30),
	"ubuntu:18.04": d(2023, time.May, 31),
	"ubuntu:20.04": d(2025, time.May, 31),
	"ubuntu:22.04": d(2027, time.April, 30),
	"ubuntu:24.04": d(2029, time.April, 30),
	"ubuntu:21.04": d(2022, time.January, 20),
	"ubuntu:21.10": d(2022, time.July, 14),
	"ubuntu:22.10": d(2023, time.July, 20),
	"ubuntu:23.04": d(2024, time.January, 25),
	"ubuntu:23.10": d(2024, time.July, 11),
	"ubuntu:24.10": d(2025, time.July, 10),
	"ubuntu:25.04": d(2026, time.January, 15),
	"ubuntu:25.10": d(2026, time.July, 10),
	"debian:9":     d(2022, time.June, 30),
	"debian:10":    d(2024, time.June, 30),
	"debian:11":    d(2026, time.August, 31),
	"debian:12":    d(2028, time.June, 30),
	"debian:13":    d(2030, time.June, 30),
}

// SupportEnd reports when regular security support for an OS release ends.
func SupportEnd(id, version string) (time.Time, bool) {
	t, ok := supportEnd[id+":"+version]
	return t, ok
}
