package gateway

// Typed response structs replace map[string]any for compile-time safety.

// ErrorResponse is the standard error envelope for all API error responses.
type ErrorResponse struct {
	Error any `json:"error"`
}

// APIError is a detailed error with type and code (OpenAI-compatible).
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// OKResponse is the standard success envelope for mutation endpoints.
type OKResponse struct {
	OK      bool   `json:"ok"`
	Enabled *bool  `json:"enabled,omitempty"`
	Result  string `json:"result,omitempty"`
}

// AcceptedResponse indicates an async task was accepted.
type AcceptedResponse struct {
	Status string `json:"status"`
}

// HealthResponse is the structured health check result.
type HealthResponse struct {
	Status  string         `json:"status"`
	Version string         `json:"version"`
	Checks  map[string]any `json:"checks"`
}

// ChannelCheck is one channel's connectivity report inside HealthResponse.
type ChannelCheck struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
}

// ModelListResponse wraps a list of model objects.
type ModelListResponse struct {
	Object string `json:"object"`
	Data   any    `json:"data"`
}

// ModelResponse describes a single model.
type ModelResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// CronListResponse wraps the job list.
type CronListResponse struct {
	Jobs any `json:"jobs"`
}

// CronJobResponse wraps a created/updated job.
type CronJobResponse struct {
	Job any `json:"job"`
}

// TaskListResponse wraps the task list.
type TaskListResponse struct {
	Tasks any `json:"tasks"`
}

// UsageSessionResponse is a single-session usage response.
type UsageSessionResponse struct {
	Session string `json:"session"`
	Usage   any    `json:"usage"`
	Calls   int    `json:"calls"`
}

// UsageAllResponse is the aggregate usage response.
type UsageAllResponse struct {
	Sessions  any `json:"sessions"`
	Aggregate any `json:"aggregate"`
}

// CronHistoryResponse wraps paginated cron run history.
type CronHistoryResponse struct {
	Entries any  `json:"entries"`
	Total   int  `json:"total"`
	HasMore bool `json:"hasMore"`
}

// ToolInvokeResponse wraps a tool execution result.
type ToolInvokeResponse struct {
	OK     bool   `json:"ok"`
	Result string `json:"result"`
}
