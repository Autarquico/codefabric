package relay

import (
	"context"
	"fmt"
	"strings"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Commit is one commit of a history read: its ID, subject line and the paths it touched.
type Commit struct {
	ID      string
	Subject string
	Files   []string
}

// Log returns up to limit commits reachable from tip and not from base, newest
// first, and whether there were more. base and tip are commit IDs. It answers
// what a Run did on top of the commit it started from.
func (r *Relay) Log(ctx context.Context, project domain.ID, base, tip string, limit int) ([]Commit, bool, error) {
	if kind, _, perr := domain.ParseID(string(project)); perr != nil || kind != domain.KindProject {
		return nil, false, fmt.Errorf("relay: %q is not a Project ID", project)
	}
	if !domain.ValidCommitID(base) || !domain.ValidCommitID(tip) || limit < 1 {
		return nil, false, fmt.Errorf("relay: log needs two commit IDs and a positive limit")
	}
	dir := r.RepoDir(project)
	out, err := r.run(ctx, verifyTimeout, dir, "-C", dir, "-c", "core.quotepath=off", "log", "--no-color",
		"--format=%x1e%H%x1f%s", "--name-only", fmt.Sprintf("--max-count=%d", limit+1), base+".."+tip)
	if err != nil {
		return nil, false, err
	}
	var commits []Commit
	for _, chunk := range strings.Split(out, "\x1e") {
		head, files, _ := strings.Cut(chunk, "\n")
		id, subject, ok := strings.Cut(head, "\x1f")
		if !ok {
			continue
		}
		c := Commit{ID: id, Subject: subject}
		for _, f := range strings.Split(files, "\n") {
			if f != "" {
				c.Files = append(c.Files, f)
			}
		}
		commits = append(commits, c)
	}
	if len(commits) > limit {
		return commits[:limit], true, nil
	}
	return commits, false, nil
}
