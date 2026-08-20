package main

import "testing"

func TestParsePullRequestURLCanonicalizesIdentity(t *testing.T) {
	pr, err := ParsePullRequestURL("https://github.com/Denifilatoff/Reviewctl/pull/19")
	if err != nil {
		t.Fatal(err)
	}
	if pr.Provider != "github" || pr.Repository != "denifilatoff/reviewctl" || pr.Number != 19 ||
		pr.URL != "https://github.com/denifilatoff/reviewctl/pull/19" {
		t.Fatalf("unexpected pull request: %+v", pr)
	}
}

func TestParsePullRequestURLRejectsNonCanonicalInput(t *testing.T) {
	for _, input := range []string{
		"http://github.com/acme/service/pull/1",
		"https://www.github.com/acme/service/pull/1",
		"https://github.com/acme/service/pull/1/",
		"https://github.com/acme/service/pull/0",
		"https://github.com/acme/service/issues/1",
		"https://github.com/acme/service/pull/1?x=1",
		"https://github.com/acme/service/pull/1?",
		"https://user@github.com/acme/service/pull/1",
		"https://github.com/%61cme/service/pull/1",
	} {
		if _, err := ParsePullRequestURL(input); err == nil {
			t.Errorf("expected %q to be rejected", input)
		}
	}
}
