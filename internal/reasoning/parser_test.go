package reasoning

import (
	"testing"
)

func TestParseResponse(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		wantExpire int
		wantCreate int
		wantErr    bool
	}{
		{
			name:       "basic valid",
			input:      `{"expire":["abc-123"],"create":[{"content":"test insight","surface_type":"insight","priority":3,"tags":["t1"]}]}`,
			wantExpire: 1,
			wantCreate: 1,
		},
		{
			name:       "empty arrays",
			input:      `{"expire":[],"create":[]}`,
			wantExpire: 0,
			wantCreate: 0,
		},
		{
			name:       "markdown code fence",
			input:      "```json\n{\"expire\":[],\"create\":[{\"content\":\"hello\",\"surface_type\":\"warning\",\"priority\":1}]}\n```",
			wantExpire: 0,
			wantCreate: 1,
		},
		{
			name:       "preamble text before JSON",
			input:      "Here is my analysis:\n{\"expire\":[],\"create\":[]}",
			wantExpire: 0,
			wantCreate: 0,
		},
		{
			name:    "no JSON at all",
			input:   "I don't have anything to surface right now.",
			wantErr: true,
		},
		{
			name:    "malformed JSON",
			input:   `{"expire":[}`,
			wantErr: true,
		},
		{
			name:       "caps at 5 surfaces",
			input:      `{"expire":[],"create":[{"content":"1","surface_type":"insight","priority":3},{"content":"2","surface_type":"insight","priority":3},{"content":"3","surface_type":"insight","priority":3},{"content":"4","surface_type":"insight","priority":3},{"content":"5","surface_type":"insight","priority":3},{"content":"6","surface_type":"insight","priority":3}]}`,
			wantExpire: 0,
			wantCreate: 5,
		},
		{
			name:       "empty content filtered out",
			input:      `{"expire":[],"create":[{"content":"","surface_type":"insight","priority":3},{"content":"real","surface_type":"insight","priority":3}]}`,
			wantExpire: 0,
			wantCreate: 1,
		},
		{
			name:       "invalid type defaults to insight",
			input:      `{"expire":[],"create":[{"content":"test","surface_type":"bogus","priority":3}]}`,
			wantExpire: 0,
			wantCreate: 1,
		},
		{
			name:       "null expire becomes empty",
			input:      `{"create":[{"content":"a","surface_type":"question","priority":2}]}`,
			wantExpire: 0,
			wantCreate: 1,
		},
		{
			name:       "trigger_at parsed",
			input:      `{"expire":[],"create":[{"content":"remind me","surface_type":"reminder","priority":2,"trigger_at":"2026-03-20T09:00:00Z"}]}`,
			wantExpire: 0,
			wantCreate: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := ParseResponse(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(resp.Expire) != tc.wantExpire {
				t.Errorf("expire count = %d, want %d", len(resp.Expire), tc.wantExpire)
			}
			if len(resp.Create) != tc.wantCreate {
				t.Errorf("create count = %d, want %d", len(resp.Create), tc.wantCreate)
			}
		})
	}
}

func TestParseResponsePriorityClamping(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantPri  int
	}{
		{"zero becomes 3", `{"expire":[],"create":[{"content":"a","surface_type":"insight","priority":0}]}`, 3},
		{"negative becomes 3", `{"expire":[],"create":[{"content":"a","surface_type":"insight","priority":-1}]}`, 3},
		{"6 clamped to 5", `{"expire":[],"create":[{"content":"a","surface_type":"insight","priority":6}]}`, 5},
		{"valid stays", `{"expire":[],"create":[{"content":"a","surface_type":"insight","priority":2}]}`, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := ParseResponse(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(resp.Create) != 1 {
				t.Fatalf("expected 1 surface, got %d", len(resp.Create))
			}
			if resp.Create[0].Priority != tc.wantPri {
				t.Errorf("priority = %d, want %d", resp.Create[0].Priority, tc.wantPri)
			}
		})
	}
}

func TestParseResponseTypeNormalization(t *testing.T) {
	validTypes := []string{"insight", "question", "warning", "reminder", "connection"}
	for _, st := range validTypes {
		t.Run(st, func(t *testing.T) {
			input := `{"expire":[],"create":[{"content":"test","surface_type":"` + st + `","priority":3}]}`
			resp, err := ParseResponse(input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.Create[0].SurfaceType != st {
				t.Errorf("surface_type = %q, want %q", resp.Create[0].SurfaceType, st)
			}
		})
	}

	// Invalid type should default to insight
	input := `{"expire":[],"create":[{"content":"test","surface_type":"invalid","priority":3}]}`
	resp, err := ParseResponse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Create[0].SurfaceType != "insight" {
		t.Errorf("invalid type should default to insight, got %q", resp.Create[0].SurfaceType)
	}
}

func TestRawSurfaceParseTriggerAt(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		wantNil bool
	}{
		{"empty", "", true},
		{"valid RFC3339", "2026-03-20T09:00:00Z", false},
		{"invalid format", "tomorrow", true},
		{"partial date", "2026-03-20", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := RawSurface{TriggerAt: tc.val}
			got := rs.ParseTriggerAt()
			if tc.wantNil && got != nil {
				t.Errorf("expected nil, got %v", got)
			}
			if !tc.wantNil && got == nil {
				t.Error("expected non-nil, got nil")
			}
		})
	}
}

func TestParseResponseNullTags(t *testing.T) {
	input := `{"expire":[],"create":[{"content":"test","surface_type":"insight","priority":3}]}`
	resp, err := ParseResponse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Create[0].Tags == nil {
		t.Error("tags should be initialized to empty slice, not nil")
	}
}
