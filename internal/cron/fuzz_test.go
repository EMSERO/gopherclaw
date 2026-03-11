package cron

import (
	"encoding/json"
	"testing"
)

// FuzzIntervalFromSpec fuzzes the cron schedule spec parser to ensure it
// never panics on arbitrary input strings.
func FuzzIntervalFromSpec(f *testing.F) {
	// Valid specs
	f.Add("@hourly")
	f.Add("@daily")
	f.Add("@weekly")
	f.Add("@every 1h")
	f.Add("@every 30m")
	f.Add("@every 1h30m")
	f.Add("@every 500ms")
	f.Add("09:00")
	f.Add("23:59")
	f.Add("00:00")

	// Invalid/edge cases
	f.Add("")
	f.Add("@")
	f.Add("@every")
	f.Add("@every ")
	f.Add("@every 0s")
	f.Add("@every -1h")
	f.Add("@monthly")
	f.Add("@yearly")
	f.Add("25:00")
	f.Add("12:60")
	f.Add("ab:cd")
	f.Add("1:2")
	f.Add("123:456")
	f.Add("  @daily  ")
	f.Add("@every abc")
	f.Add("* * * * *")
	f.Add("@every 999999999h")

	f.Fuzz(func(t *testing.T, spec string) {
		// Must not panic.
		_, _ = intervalFromSpec(spec)
	})
}

// FuzzNextRun fuzzes the nextRun schedule calculator.
func FuzzNextRun(f *testing.F) {
	f.Add("@hourly")
	f.Add("@daily")
	f.Add("@weekly")
	f.Add("@every 1h")
	f.Add("09:00")
	f.Add("")
	f.Add("@every 0s")
	f.Add("@every -1h")
	f.Add("garbage")
	f.Add("@every")
	f.Add("  @daily  ")

	f.Fuzz(func(t *testing.T, spec string) {
		// Must not panic.
		_, _ = nextRun(spec)
	})
}

// FuzzJobUnmarshal fuzzes JSON deserialization of Job structs to ensure
// no panics on arbitrary JSON input.
func FuzzJobUnmarshal(f *testing.F) {
	f.Add([]byte(`{"id":"test","spec":"@daily","instruction":"do something","enabled":true}`))
	f.Add([]byte(`{"id":"full","schedule":{"kind":"every","everyMs":3600000},"payload":{"kind":"agentTurn","message":"run"}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"id":"","enabled":false}`))
	f.Add([]byte(`{"schedule":{"kind":"every","everyMs":-1}}`))
	f.Add([]byte(``))
	f.Add([]byte(`{`))
	f.Add([]byte(`{"delivery":{"mode":"announce","channel":"last"}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var j Job
		// Must not panic.
		_ = json.Unmarshal(data, &j)
	})
}

// FuzzJobsFileUnmarshal fuzzes the full jobs.json file format parsing.
func FuzzJobsFileUnmarshal(f *testing.F) {
	f.Add([]byte(`{"version":1,"jobs":[{"id":"a","schedule":{"kind":"every","everyMs":60000}}]}`))
	f.Add([]byte(`{"version":1,"jobs":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"version":999,"jobs":[{}]}`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, data []byte) {
		var file struct {
			Version int    `json:"version"`
			Jobs    []*Job `json:"jobs"`
		}
		// Must not panic.
		_ = json.Unmarshal(data, &file)
	})
}
