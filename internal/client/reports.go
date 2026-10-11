package client

import (
	"context"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// ReportRun tells the Controller what became of a Run this Worker executes:
// 1 to v1.MaxReportEntries entries of that Run, in the order of their
// WorkerSeq. It is for clients opened with a Worker's credentials. The answer
// has an outcome for each entry; a rejection stops the batch there.
func (c *Client) ReportRun(ctx context.Context, run string, req v1.RunReportRequest) (v1.RunReportResponse, error) {
	var out v1.RunReportResponse
	err := c.do(ctx, http.MethodPost, refPath("worker/runs", run, "report"), req, &out)
	return out, err
}
