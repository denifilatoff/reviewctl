package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type PullRequest struct {
	Provider   string `json:"provider"`
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
	URL        string `json:"url"`
}

func ParsePullRequestURL(raw string) (PullRequest, error) {
	var pr PullRequest
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawPath != "" ||
		u.ForceQuery || u.RawQuery != "" || u.Fragment != "" {
		return pr, fmt.Errorf("pull request URL must be canonical GitHub HTTPS URL")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" || !repositoryPattern.MatchString(parts[0]+"/"+parts[1]) {
		return pr, fmt.Errorf("pull request URL must match https://github.com/owner/repository/pull/number")
	}
	number, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || number < 1 {
		return pr, fmt.Errorf("pull request number must be positive")
	}
	repository := strings.ToLower(parts[0] + "/" + parts[1])
	pr = PullRequest{
		Provider:   "github",
		Repository: repository,
		Number:     number,
		URL:        fmt.Sprintf("https://github.com/%s/pull/%d", repository, number),
	}
	if raw != strings.TrimSuffix(raw, "/") {
		return PullRequest{}, fmt.Errorf("pull request URL must not end with a slash")
	}
	return pr, nil
}

func samePullRequestIdentity(raw string, expected PullRequest) bool {
	actual, err := ParsePullRequestURL(raw)
	return err == nil && actual.Provider == expected.Provider && actual.Repository == expected.Repository &&
		actual.Number == expected.Number
}
