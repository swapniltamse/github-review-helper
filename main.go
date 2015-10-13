package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/tylerb/graceful.v1"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

const (
	githubStatusSquashContext     = "review/squash"
	githubStatusPeerReviewContext = "review/peer"
)

type (
	Issue struct {
		Number     int
		Repository Repository
	}

	Issueable interface {
		Issue() Issue
	}

	IssueComment struct {
		IssueNumber   int
		Comment       string
		IsPullRequest bool
		Repository    Repository
	}

	PullRequestEvent struct {
		IssueNumber int
		Action      string
		Repository  Repository
	}

	Repository struct {
		Owner string
		Name  string
		URL   string
	}
)

func (i IssueComment) Issue() Issue {
	return Issue{
		Number:     i.IssueNumber,
		Repository: i.Repository,
	}
}

func (p PullRequestEvent) Issue() Issue {
	return Issue{
		Number:     p.IssueNumber,
		Repository: p.Repository,
	}
}

func main() {
	conf := NewConfig()
	githubClient := initGithubClient(conf.AccessToken)
	reposDir, err := ioutil.TempDir("", "github-review-helper")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(reposDir)

	git := NewGit(reposDir)

	mux := http.NewServeMux()
	mux.Handle("/", CreateHandler(conf, git, githubClient.PullRequests, githubClient.Repositories))

	graceful.Run(fmt.Sprintf(":%d", conf.Port), 10*time.Second, mux)
}

func CreateHandler(conf Config, git Git, pullRequests PullRequests, repositories Repositories) Handler {
	return func(w http.ResponseWriter, r *http.Request) Response {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			return ErrorResponse{err, http.StatusInternalServerError, "Failed to read the request's body"}
		}
		signature := r.Header.Get("X-Hub-Signature")
		if signature == "" {
			return ErrorResponse{nil, http.StatusUnauthorized, "Please provide a X-Hub-Signature"}
		}
		hasSecret, err := hasSecret(body, signature, conf.Secret)
		if err != nil {
			return ErrorResponse{err, http.StatusInternalServerError, "Failed to check the signature"}
		} else if !hasSecret {
			return ErrorResponse{nil, http.StatusForbidden, "Bad X-Hub-Signature"}
		}
		eventType := r.Header.Get("X-Github-Event")
		switch eventType {
		case "issue_comment":
			return handleIssueComment(w, body, git, pullRequests, repositories)
		case "pull_request":
			return handlePullRequest(w, body, pullRequests, repositories)
		}
		return SuccessResponse{"Not an event I understand. Ignoring."}
	}
}

// startsWithPlusOne matches strings that start with either a +1 (not followed by other digits) or a :+1: emoji
var startsWithPlusOne = regexp.MustCompile(`^(:\+1:|\+1($|\D))`)

func handleIssueComment(w http.ResponseWriter, body []byte, git Git, pullRequests PullRequests, repositories Repositories) Response {
	issueComment, err := parseIssueComment(body)
	if err != nil {
		return ErrorResponse{err, http.StatusInternalServerError, "Failed to parse the request's body"}
	}
	if !issueComment.IsPullRequest {
		return SuccessResponse{"Not a PR. Ignoring."}
	}
	switch {
	case issueComment.Comment == "!squash":
		return handleSquash(w, issueComment, git, pullRequests, repositories)
	case startsWithPlusOne.MatchString(issueComment.Comment):
		return handlePlusOne(w, issueComment, pullRequests, repositories)
	}
	return SuccessResponse{"Not a command I understand. Ignoring."}
}

func handleSquash(w http.ResponseWriter, issueComment IssueComment, git Git, pullRequests PullRequests, repositories Repositories) Response {
	pr, errResp := getPR(issueComment, pullRequests)
	if errResp != nil {
		return errResp
	}
	log.Printf("Squashing %s that's going to be merged into %s\n", *pr.Head.Ref, *pr.Base.Ref)
	repo, err := git.GetUpdatedRepo(issueComment.Repository.URL, issueComment.Repository.Owner, issueComment.Repository.Name)
	if err != nil {
		return ErrorResponse{err, http.StatusInternalServerError, "Failed to update the local repo"}
	}
	if err = repo.RebaseAutosquash(*pr.Base.SHA, *pr.Head.SHA); err != nil {
		log.Printf("Failed to autosquash the commits with an interactive rebase: %s. Setting a failure status.\n", err)
		status := createSquashStatus("failure", "Failed to automatically squash the fixup! and squash! commits. Please squash manually")
		if errResp := setStatus(issueComment.Repository, *pr.Head.SHA, status, repositories); errResp != nil {
			return errResp
		}
		return SuccessResponse{"Failed to autosquash the commits with an interactive rebase. Reported the failure."}
	}
	if err = repo.ForcePushHeadTo(*pr.Head.Ref); err != nil {
		return ErrorResponse{err, http.StatusInternalServerError, "Failed to push the squashed version"}
	}
	squashedHeadSHA, err := repo.GetHeadSHA()
	if err != nil {
		return ErrorResponse{err, http.StatusInternalServerError, "Failed to get the squashed branch's HEAD's SHA"}
	}
	status := createSquashStatus("success", "All fixup! and squash! commits successfully squashed")
	if errResp := setStatus(issueComment.Repository, squashedHeadSHA, status, repositories); errResp != nil {
		return errResp
	}
	return SuccessResponse{}
}

func handlePlusOne(w http.ResponseWriter, issueComment IssueComment, pullRequests PullRequests, repositories Repositories) Response {
	log.Printf("Marking PR %s as peer reviewed\n", issueComment.Issue().FullName())
	status := createPeerReviewStatus("success", "This PR has been peer reviewed")
	if errResp := setPRHeadStatus(issueComment, status, pullRequests, repositories); errResp != nil {
		return errResp
	}
	return SuccessResponse{}
}

func handlePullRequest(w http.ResponseWriter, body []byte, pullRequests PullRequests, repositories Repositories) Response {
	pullRequestEvent, err := parsePullRequestEvent(body)
	if err != nil {
		return ErrorResponse{err, http.StatusInternalServerError, "Failed to parse the request's body"}
	}
	if !(pullRequestEvent.Action == "opened" || pullRequestEvent.Action == "synchronize") {
		return SuccessResponse{"PR not opened or synchronized. Ignoring."}
	}
	log.Printf("Checking for fixup commits for PR %s.\n", pullRequestEvent.Issue().FullName())
	commits, errResp := getCommits(pullRequestEvent, pullRequests)
	if errResp != nil {
		return errResp
	}
	if !includesFixupCommits(commits) {
		return SuccessResponse{}
	}
	status := createSquashStatus("pending", "This PR needs to be squashed with !squash before merging")
	if errResp := setPRHeadStatus(pullRequestEvent, status, pullRequests, repositories); errResp != nil {
		return errResp
	}
	return SuccessResponse{}
}

func includesFixupCommits(commits []github.RepositoryCommit) bool {
	for _, commit := range commits {
		if strings.HasPrefix(*commit.Commit.Message, "fixup! ") || strings.HasPrefix(*commit.Commit.Message, "squash! ") {
			return true
		}
	}
	return false
}

func setPRHeadStatus(issueable Issueable, status *github.RepoStatus, pullRequests PullRequests, repositories Repositories) *ErrorResponse {
	pr, errResp := getPR(issueable, pullRequests)
	if errResp != nil {
		return errResp
	}
	repository := issueable.Issue().Repository
	return setStatus(repository, *pr.Head.SHA, status, repositories)
}

func setStatus(repository Repository, commitRef string, status *github.RepoStatus, repositories Repositories) *ErrorResponse {
	_, _, err := repositories.CreateStatus(repository.Owner, repository.Name, commitRef, status)
	if err != nil {
		message := fmt.Sprintf("Failed to create a %s status for commit %s", *status.State, commitRef)
		return &ErrorResponse{err, http.StatusBadGateway, message}
	}
	return nil
}

func getPR(issueable Issueable, pullRequests PullRequests) (*github.PullRequest, *ErrorResponse) {
	issue := issueable.Issue()
	pr, _, err := pullRequests.Get(issue.Repository.Owner, issue.Repository.Name, issue.Number)
	if err != nil {
		message := fmt.Sprintf("Getting PR %s failed", issue.FullName())
		return nil, &ErrorResponse{err, http.StatusBadGateway, message}
	}
	return pr, nil
}

func getCommits(issueable Issueable, pullRequests PullRequests) ([]github.RepositoryCommit, *ErrorResponse) {
	issue := issueable.Issue()
	commits, _, err := pullRequests.ListCommits(issue.Repository.Owner, issue.Repository.Name, issue.Number, nil)
	if err != nil {
		message := fmt.Sprintf("Getting commits for PR %s failed", issue.FullName())
		return nil, &ErrorResponse{err, http.StatusBadGateway, message}
	}
	return commits, nil
}

func createPeerReviewStatus(state, description string) *github.RepoStatus {
	return &github.RepoStatus{
		State:       github.String(state),
		Description: github.String(description),
		Context:     github.String(githubStatusPeerReviewContext),
	}
}

func createSquashStatus(state, description string) *github.RepoStatus {
	return &github.RepoStatus{
		State:       github.String(state),
		Description: github.String(description),
		Context:     github.String(githubStatusSquashContext),
	}
}

func initGithubClient(accessToken string) *github.Client {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: accessToken},
	)
	tc := oauth2.NewClient(oauth2.NoContext, ts)
	return github.NewClient(tc)
}

func parseIssueComment(body []byte) (IssueComment, error) {
	var message struct {
		Issue struct {
			Number      int `json:"Number"`
			PullRequest struct {
				URL string `json:"url"`
			} `json:"pull_request"`
		} `json:"issue"`
		Repository struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
			SSHURL string `json:"ssh_url"`
		} `json:"repository"`
		Comment struct {
			Body string `json:"body"`
		} `json:"comment"`
	}
	err := json.Unmarshal(body, &message)
	if err != nil {
		return IssueComment{}, err
	}
	return IssueComment{
		IssueNumber:   message.Issue.Number,
		Comment:       message.Comment.Body,
		IsPullRequest: message.Issue.PullRequest.URL != "",
		Repository: Repository{
			Owner: message.Repository.Owner.Login,
			Name:  message.Repository.Name,
			URL:   message.Repository.SSHURL,
		},
	}, nil
}

func parsePullRequestEvent(body []byte) (PullRequestEvent, error) {
	var message struct {
		Action      string `json:"action"`
		Number      int    `json:"number"`
		PullRequest struct {
			URL string `json:"url"`
		} `json:"pull_request"`
		Repository struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
			SSHURL string `json:"ssh_url"`
		} `json:"repository"`
	}
	err := json.Unmarshal(body, &message)
	if err != nil {
		return PullRequestEvent{}, err
	}
	return PullRequestEvent{
		IssueNumber: message.Number,
		Action:      message.Action,
		Repository: Repository{
			Owner: message.Repository.Owner.Login,
			Name:  message.Repository.Name,
			URL:   message.Repository.SSHURL,
		},
	}, nil
}

func hasSecret(message []byte, signature, key string) (bool, error) {
	var messageMACString string
	fmt.Sscanf(signature, "sha1=%s", &messageMACString)
	messageMAC, err := hex.DecodeString(messageMACString)
	if err != nil {
		return false, err
	}

	mac := hmac.New(sha1.New, []byte(key))
	mac.Write(message)
	expectedMAC := mac.Sum(nil)
	return hmac.Equal(messageMAC, expectedMAC), nil
}

func (i Issue) FullName() string {
	return fmt.Sprintf("%s/%s#%d", i.Repository.Owner, i.Repository.Name, i.Number)
}
