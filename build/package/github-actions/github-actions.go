package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/launchdarkly/ld-find-code-refs/internal/log"
	o "github.com/launchdarkly/ld-find-code-refs/internal/options"
	"github.com/launchdarkly/ld-find-code-refs/pkg/coderefs"
)

func main() {
	debug, err := o.GetDebugOptionFromEnv()
	// init logging before checking error because we need to log the error if there is one
	log.Init(debug)
	if err != nil {
		log.Error.Fatalf("error parsing debug option: %s", err)
	}

	log.Info.Printf("Setting GitHub action env vars")
	ghRepo := strings.Split(os.Getenv("GITHUB_REPOSITORY"), "/")
	if len(ghRepo) < 2 {
		log.Error.Fatalf("unable to validate GitHub repository name: %s", ghRepo)
	}
	ghBranch, err := parseBranch(os.Getenv("GITHUB_REF"))
	if err != nil {
		log.Error.Fatalf("error parsing GITHUB_REF: %s", err)
	}
	event, err := parseEvent(os.Getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		log.Error.Fatalf("error parsing GitHub event payload at %s: %s", os.Getenv("GITHUB_EVENT_PATH"), err)
	}

	options := map[string]string{
		"branch":           ghBranch,
		"repoType":         "github",
		"repoName":         ghRepo[1],
		"dir":              os.Getenv("GITHUB_WORKSPACE"),
		"updateSequenceId": strconv.FormatInt(event.Repo.PushedAt*1000, 10), // seconds to milliseconds
		"repoUrl":          event.Repo.Url,
	}
	ldOptions, err := o.GetLDOptionsFromEnv()
	if err != nil {
		log.Error.Fatalf("Error setting options: %s", err)
	}
	for k, v := range ldOptions {
		options[k] = v
	}

	if options["defaultBranch"] == "" {
		options["defaultBranch"] = event.Repo.DefaultBranch
	}

	err = o.Populate()
	if err != nil {
		log.Error.Fatalf("could not set options: %v", err)
	}
	for k, v := range options {
		err := flag.Set(k, v)
		if err != nil {
			log.Error.Fatalf("could not set option %s: %s", k, err)
		}
	}
	// Don't log ld access token
	optionsForLog := options
	optionsForLog["accessToken"] = ""
	log.Info.Printf("starting repo parsing program with options:\n %+v\n", options)
	coderefs.Scan()
}

type Event struct {
	Repo   `json:"repository"`
	Sender `json:"sender"`
}

type Repo struct {
	Url           string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	PushedAt      int64  `json:"pushed_at"`
}

type Sender struct {
	Username string `json:"login"`
}

func parseEvent(path string) (*Event, error) {
	/* #nosec */
	eventJsonFile, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	eventJsonBytes, err := ioutil.ReadAll(eventJsonFile)
	if err != nil {
		return nil, err
	}
	var evt Event
	err = json.Unmarshal(eventJsonBytes, &evt)
	if err != nil {
		return nil, err
	}
	return &evt, err
}

func parseBranch(ref string) (string, error) {
	re := regexp.MustCompile(`^refs/heads/(.+)$`)
	results := re.FindStringSubmatch(ref)

	if results == nil {
		return "", fmt.Errorf("expected branch name starting with refs/heads/, got: %s", ref)
	}

	return results[1], nil
}
