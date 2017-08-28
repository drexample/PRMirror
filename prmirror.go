package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/github"
)

// MirroredPR contains the upstream and downstream PR ids
type MirroredPR struct {
	DownstreamID int
	UpstreamID   int
}

// PRMirror contains various different variables
type PRMirror struct {
	GitHubClient  *github.Client
	Context       *context.Context
	Configuration *Config
	Database      *Database
}

// GitHubEventMonitor passes in an instance of the PRMirror struct to all HTTP calls to the webhook listener
type GitHubEventMonitor struct {
	Mirrorer PRMirror
}

// HandleEvent handles github events and acts like an event handler
func (p PRMirror) HandleEvent(event *github.Event) {
	seenEvent, _ := p.Database.SeenEvent(event.GetID())
	if seenEvent {
		return
	}

	eventType := event.GetType()
	if eventType != "PullRequestEvent" {
		return
	}

	prEvent := github.PullRequestEvent{}
	err := json.Unmarshal(event.GetRawPayload(), &prEvent)
	if err != nil {
		panic(err)
	}

	p.HandlePREvent(&prEvent)
	p.Database.AddEvent(event.GetID())
}

func (p PRMirror) HandlePREvent(prEvent *github.PullRequestEvent) {
	repoName := prEvent.Repo.GetName()
	repoOwner := prEvent.Repo.Owner.GetName()
	prEventURL := prEvent.PullRequest.GetURL()

	if repoName != p.Configuration.DownstreamRepo {
		log.Warningf("Ignoring PR Event: %s because %s != %s\n", prEventURL, repoName, p.Configuration.DownstreamRepo)
		return
	} else if repoOwner != p.Configuration.DownstreamOwner {
		log.Warningf("Ignoring PR Event: %s because %s != %s\n", prEventURL, repoOwner, p.Configuration.DownstreamOwner)
		return
	}

	log.Debugf("Handling PR Event: %s\n", prEventURL)

	prAction := prEvent.GetAction()
	if prAction == "closed" && prEvent.PullRequest.GetMerged() == true {
		prID, err := p.MirrorPR(prEvent.PullRequest)
		if err != nil {
			log.Errorf("Error while creating a new PR: %s\n", err.Error())
		} else {
			p.AddLabels(prID, []string{"Upstream PR Merged"})
			p.Database.StoreMirror(prID, prEvent.PullRequest.GetNumber())
		}
	}
}

// RunEventScraper runs the GitHub repo event API scraper
func (p PRMirror) RunEventScraper() {
	for {
		events, pollInterval, err := p.GetRepoEvents()
		if err == nil {
			for _, event := range events {
				p.HandleEvent(event)
			}
		}

		log.Debugf("Sleeping for %d as specified by GitHub\n", pollInterval)
		time.Sleep(time.Duration(pollInterval) * time.Second)
	}
}

// ServeHTTP handles HTTP requests to the webhook endpoint
func (s GitHubEventMonitor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	payload, err := github.ValidatePayload(r, []byte(s.Mirrorer.Configuration.WebhookSecret))
	if err != nil {
		log.Errorf("Error validating the payload\n")
		return
	}
	event, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		log.Errorf("Error parsing the payload\n")
	}

	switch event := event.(type) {
	case *github.PullRequestEvent:
		s.Mirrorer.HandlePREvent(event)
	}
}

// RunWebhookListener acts a webhook listener which GitHub will call with events
func (p PRMirror) RunWebhookListener() {
	server := GitHubEventMonitor{Mirrorer: p}
	err := http.ListenAndServe(fmt.Sprintf(":%d", p.Configuration.WebhookPort), server)
	log.Fatal(err)
}

// MirrorPR will mirror a PR from an upstream to the downstream
func (p PRMirror) MirrorPR(pr *github.PullRequest) (int, error) {
	log.Infof("Mirroring PR [%d]: %s from %s\n", pr.GetNumber(), pr.GetTitle(), pr.User.GetLogin())

	cmd := exec.Command(fmt.Sprintf("%s%s", p.Configuration.RepoPath, p.Configuration.ToolPath), strconv.Itoa(pr.GetNumber()), pr.GetTitle())
	cmd.Dir = p.Configuration.RepoPath
	cmdoutput, err := cmd.CombinedOutput()
	if err != nil {
		panic(err)
	}

	log.Debug(cmdoutput)

	base := "master"
	head := fmt.Sprintf("upstream-merge-%d", pr.GetNumber())
	maintainerCanModify := true // We are the owner of the PR so we can specify this as true
	title := fmt.Sprintf("[MIRROR] %s", pr.GetTitle())
	body := fmt.Sprintf("Original PR: %s\n--------------------\n%s", pr.GetHTMLURL(), strings.Replace(pr.GetBody(), "@", "@ ", -1))

	newPR := github.NewPullRequest{}
	newPR.Title = &title
	newPR.Body = &body
	newPR.Base = &base
	newPR.Head = &head
	newPR.MaintainerCanModify = &maintainerCanModify

	pr, _, err = p.GitHubClient.PullRequests.Create(*p.Context, p.Configuration.DownstreamOwner, p.Configuration.DownstreamRepo, &newPR)
	if err != nil {
		return 0, err
	}

	if strings.Contains(string(cmdoutput), "Rejected hunk") {
		p.AddLabels(pr.GetNumber(), []string{"Auto Merge Rejections"})
	}

	return pr.GetNumber(), nil
}
