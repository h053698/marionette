package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type JobRequest struct {
	Selection      Selection        `json:"selection"`
	Steps          []map[string]any `json:"steps"`
	TimeoutSeconds int              `json:"timeoutSeconds,omitempty"`
}
type Job struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	Selection      Selection         `json:"selection"`
	Steps          []map[string]any  `json:"steps"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	CreatedAt      time.Time         `json:"createdAt"`
	StartedAt      *time.Time        `json:"startedAt,omitempty"`
	FinishedAt     *time.Time        `json:"finishedAt,omitempty"`
	DeviceID       string            `json:"deviceId,omitempty"`
	Completed      int               `json:"completed"`
	Results        []json.RawMessage `json:"results"`
	Error          string            `json:"error,omitempty"`
	ErrorCode      string            `json:"errorCode,omitempty"`
}

func (c *Client) SubmitJob(ctx context.Context, request JobRequest) (Job, error) {
	var job Job
	data, _, err := c.request(ctx, "POST", "/api/jobs", "", request)
	if err == nil {
		err = json.Unmarshal(data, &job)
	}
	return job, err
}
func (c *Client) Job(ctx context.Context, id string) (Job, error) {
	var job Job
	if id == "" || strings.ContainsAny(id, "/\\.\x00\r\n") {
		return job, errors.New("invalid job ID")
	}
	data, _, err := c.request(ctx, "GET", "/api/jobs/"+url.PathEscape(id), "", nil)
	if err == nil {
		err = json.Unmarshal(data, &job)
	}
	return job, err
}

// CancelJob cancels an active job or deletes a terminal job and its artifacts.
func (c *Client) CancelJob(ctx context.Context, id string) error {
	if id == "" || strings.ContainsAny(id, "/\\.\x00\r\n") {
		return errors.New("invalid job ID")
	}
	_, _, err := c.request(ctx, "DELETE", "/api/jobs/"+url.PathEscape(id), "", nil)
	return err
}

func (c *Client) JobArtifact(ctx context.Context, id string, index int) ([]byte, error) {
	if id == "" || strings.ContainsAny(id, "/\\.\x00\r\n") || index < 0 || index > 99 {
		return nil, errors.New("invalid job ID or artifact index")
	}
	data, contentType, err := c.request(ctx, "GET", "/api/jobs/"+url.PathEscape(id)+"/artifacts/"+strconv.Itoa(index), "", nil)
	if err == nil && !strings.HasPrefix(contentType, "image/png") {
		return nil, errors.New("job artifact is not a PNG")
	}
	return data, err
}
