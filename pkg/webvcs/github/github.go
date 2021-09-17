package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v35/github"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/params/info"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/webvcs"
	"golang.org/x/oauth2"
)

type VCS struct {
	Client        *github.Client
	Token, APIURL string
}

// NewGithubVCS Create a new GitHub VCS object for token
func NewGithubVCS(ctx context.Context, info info.PacOpts) VCS {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: info.VCSToken},
	)
	tc := oauth2.NewClient(ctx, ts)

	var client *github.Client
	apiURL := info.VCSAPIURL
	if apiURL != "" {
		if !strings.HasPrefix(apiURL, "https") {
			apiURL = "https://" + apiURL
		}
		client, _ = github.NewEnterpriseClient(apiURL, apiURL, tc)
	} else {
		client = github.NewClient(tc)
	}

	return VCS{
		Client: client,
		Token:  info.VCSToken,
		APIURL: apiURL,
	}
}

// concatAllYamlFiles concat all yaml files from a directory as one big multi document yaml string
func (v VCS) concatAllYamlFiles(ctx context.Context, objects []*github.RepositoryContent, runevent *info.Event) (string, error) {
	var allTemplates string

	for _, value := range objects {
		if strings.HasSuffix(value.GetName(), ".yaml") ||
			strings.HasSuffix(value.GetName(), ".yml") {
			data, err := v.getObject(ctx, value.GetSHA(), runevent)
			if err != nil {
				return "", err
			}
			if allTemplates != "" && !strings.HasPrefix(string(data), "---") {
				allTemplates += "---"
			}
			allTemplates += "\n" + string(data) + "\n"
		}
	}
	return allTemplates, nil
}

// GetTektonDir Get all yaml files in tekton directory return as a single concated file
func (v VCS) GetTektonDir(ctx context.Context, runevent *info.Event, path string) (string, error) {
	fp, objects, resp, err := v.Client.Repositories.GetContents(ctx, runevent.Owner,
		runevent.Repository, path, &github.RepositoryContentGetOptions{Ref: runevent.SHA})

	if fp != nil {
		return "", fmt.Errorf("the object %s is a file instead of a directory", path)
	}
	if resp != nil && resp.Response.StatusCode == http.StatusNotFound {
		return "", nil
	}

	if err != nil {
		return "", err
	}

	return v.concatAllYamlFiles(ctx, objects, runevent)
}

// getPullRequest get a pull request details
func (v VCS) getPullRequest(ctx context.Context, runevent *info.Event, prNumber int) (*info.Event, error) {
	pr, _, err := v.Client.PullRequests.Get(ctx, runevent.Owner, runevent.Repository, prNumber)
	if err != nil {
		return runevent, err
	}
	// Make sure to use the Base for Default BaseBranch or there would be a potential hijack
	runevent.DefaultBranch = pr.GetBase().GetRepo().GetDefaultBranch()
	runevent.URL = pr.GetBase().GetRepo().GetHTMLURL()
	runevent.SHA = pr.GetHead().GetSHA()
	runevent.SHAURL = fmt.Sprintf("%s/commit/%s", pr.GetHTMLURL(), pr.GetHead().GetSHA())
	// TODO: Maybe if we wanted to allow rerequest from non approved user we
	// would use the CheckRun Sender instead of the rerequest sender, could it
	// be a room for abuse? 🤔
	runevent.Sender = pr.GetUser().GetLogin()
	runevent.HeadBranch = pr.GetHead().GetRef()
	runevent.BaseBranch = pr.GetBase().GetRef()
	runevent.EventType = "pull_request"
	return runevent, nil
}

// populateCommitInfo get info on a commit in runevent
func (v VCS) populateCommitInfo(ctx context.Context, runevent *info.Event) error {
	commit, _, err := v.Client.Git.GetCommit(ctx, runevent.Owner, runevent.Repository, runevent.SHA)
	if err != nil {
		return err
	}

	runevent.SHAURL = commit.GetHTMLURL()
	runevent.SHATitle = strings.Split(commit.GetMessage(), "\n\n")[0]

	return nil
}

// GetFileInsideRepo Get a file via Github API using the runinfo information, we
// branch is true, the user the branch as ref isntead of the SHA
// TODO: merge GetFileInsideRepo amd GetTektonDir
func (v VCS) GetFileInsideRepo(ctx context.Context, runevent *info.Event, path string, branch bool) (string, error) {
	ref := runevent.SHA
	if branch {
		ref = runevent.BaseBranch
	}

	fp, objects, resp, err := v.Client.Repositories.GetContents(ctx, runevent.Owner,
		runevent.Repository, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return "", err
	}
	if objects != nil {
		return "", fmt.Errorf("referenced file inside the Github Repository %s is a directory", path)
	}
	if resp.Response.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("cannot find %s in this repository", path)
	}

	getobj, err := v.getObject(ctx, fp.GetSHA(), runevent)
	if err != nil {
		return "", err
	}

	return string(getobj), nil
}

// getObject Get an object from a repository
func (v VCS) getObject(ctx context.Context, sha string, runevent *info.Event) ([]byte, error) {
	blob, _, err := v.Client.Git.GetBlob(ctx, runevent.Owner, runevent.Repository, sha)
	if err != nil {
		return nil, err
	}

	decoded, err := base64.StdEncoding.DecodeString(blob.GetContent())
	if err != nil {
		return nil, err
	}
	return decoded, err
}

func (v VCS) createCheckRun(ctx context.Context, status string, runevent *info.Event, pacopts info.PacOpts) (*github.CheckRun, error) {
	now := github.Timestamp{Time: time.Now()}
	checkrunoption := github.CreateCheckRunOptions{
		Name:       pacopts.ApplicationName,
		HeadSHA:    runevent.SHA,
		Status:     github.String(status),
		DetailsURL: github.String(pacopts.LogURL),
		StartedAt:  &now,
	}

	checkRun, _, err := v.Client.Checks.CreateCheckRun(ctx, runevent.Owner, runevent.Repository, checkrunoption)
	return checkRun, err
}

func (v VCS) CreateStatus(ctx context.Context, runevent *info.Event, pacopts info.PacOpts, status webvcs.StatusOpts) error {
	checkrunid := runevent.CheckRunID

	// TODO: get console info ASAP,
	// Create initial checkrun if not exist
	if checkrunid == nil {
		createCheckRun, err := v.createCheckRun(ctx, "in_progress", runevent, pacopts)
		if err != nil {
			return err
		}
		checkrunid = createCheckRun.ID
	}

	now := github.Timestamp{Time: time.Now()}

	var summary, title string

	switch status.Conclusion {
	case "success":
		title = "✅ Success"
		summary = fmt.Sprintf("%s has successfully validated your commit.", pacopts.ApplicationName)
	case "failure":
		title = "❌ Failed"
		summary = fmt.Sprintf("%s has <b>failed</b>.", pacopts.ApplicationName)
	case "skipped":
		title = "➖ Skipped"
		summary = fmt.Sprintf("%s is skipping this commit.", pacopts.ApplicationName)
	case "neutral":
		title = "❓ Unknown"
		summary = fmt.Sprintf("%s doesn't know what happened with this commit.", pacopts.ApplicationName)
	}

	if status.Status == "in_progress" {
		title = "CI has Started"
		summary = fmt.Sprintf("%s is running.", pacopts.ApplicationName)
	}

	checkRunOutput := &github.CheckRunOutput{
		Title:   &title,
		Summary: &summary,
		Text:    &status.Text,
	}

	opts := github.UpdateCheckRunOptions{
		Name:   pacopts.ApplicationName,
		Status: &status.Status,
		Output: checkRunOutput,
	}

	if status.DetailsURL != "" {
		opts.DetailsURL = &status.DetailsURL
	}

	// Only set completed-at if conclusion is set (which means finished)
	if status.Conclusion != "" {
		opts.CompletedAt = &now
		opts.Conclusion = &status.Conclusion
	}

	_, _, err := v.Client.Checks.UpdateCheckRun(ctx, runevent.Owner, runevent.Repository, *checkrunid, opts)
	return err
}
