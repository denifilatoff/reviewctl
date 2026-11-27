package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func fakeGHReviewPage(nodes []any, after string) map[string]any {
	size := 100
	if value := os.Getenv("REVIEWCTL_FAKE_GRAPHQL_PAGE_SIZE"); value != "" {
		size, _ = strconv.Atoi(value)
	}
	start := 0
	if after != "" {
		var err error
		start, err = strconv.Atoi(after)
		if err != nil || start <= 0 || start >= len(nodes) {
			os.Exit(76)
		}
	}
	end := min(len(nodes), start+size)
	cursor := strconv.Itoa(end)
	if after != "" {
		switch os.Getenv("REVIEWCTL_FAKE_GRAPHQL_PAGE_FAULT") {
		case "repeated cursor":
			cursor = after
		case "missing cursor":
			cursor = ""
		case "partial page":
			return map[string]any{"nodes": nodes[start:end]}
		}
	}
	return map[string]any{
		"nodes":    nodes[start:end],
		"pageInfo": map[string]any{"hasNextPage": end < len(nodes), "endCursor": cursor},
	}
}

func TestListGitHubReviewThreadsFollowsThreadAndCommentCursors(t *testing.T) {
	_, pr, _, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_GRAPHQL_PAGE_SIZE", "1")
	want := []githubReviewThread{
		{ID: "PRRT_resolved", IsResolved: true, Comments: []githubReviewComment{
			{DatabaseID: 1, Author: "reviewer", Body: "Finding."},
			{DatabaseID: 2, Author: "author", ReplyToID: 1, Body: "Fix applied."},
			{DatabaseID: 3, Author: "reviewer", ReplyToID: 1, Body: "Fix verified."},
		}},
		{ID: "PRRT_unowned", Comments: []githubReviewComment{{DatabaseID: 4, Author: "someone-else"}}},
		{ID: "PRRT_open", Comments: []githubReviewComment{
			{DatabaseID: 5, Author: "reviewer"},
			{DatabaseID: 6, Author: "author", ReplyToID: 5},
		}},
	}
	writeFakeReviewThreads(t, temp, want)

	got, err := listGitHubReviewThreads(context.Background(), pr)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("listGitHubReviewThreads(%v) = %+v, %v, want %+v", pr, got, err, want)
	}
}

func TestGitHubReviewReadersRejectPartialPages(t *testing.T) {
	for _, tc := range []struct {
		fault string
		want  string
	}{
		{"repeated cursor", "repeated a pagination cursor"},
		{"missing cursor", "no cursor for the next page"},
		{"partial page", "incomplete page"},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			_, pr, _, temp := publicationFixture(t)
			t.Setenv("REVIEWCTL_FAKE_GRAPHQL_PAGE_SIZE", "1")
			t.Setenv("REVIEWCTL_FAKE_GRAPHQL_PAGE_FAULT", tc.fault)
			writeFakeReviewThreads(t, temp, []githubReviewThread{
				{ID: "PRRT_one", Comments: []githubReviewComment{{DatabaseID: 1, Author: "reviewer"}}},
				{ID: "PRRT_two", Comments: []githubReviewComment{{DatabaseID: 2, Author: "reviewer"}}},
				{ID: "PRRT_three", Comments: []githubReviewComment{
					{DatabaseID: 3, Author: "reviewer"},
					{DatabaseID: 4, Author: "author", ReplyToID: 3},
					{DatabaseID: 5, Author: "reviewer", ReplyToID: 3},
				}},
			})
			ctx := context.Background()
			threads, err := listGitHubReviewThreads(ctx, pr)
			var coded *codedError
			if threads != nil || !errors.As(err, &coded) || coded.code != "github_failed" || !strings.Contains(coded.message, tc.want) {
				t.Errorf("listGitHubReviewThreads(%v) = %+v, %v, want nil and github_failed with %q", pr, threads, err, tc.want)
			}
			thread, err := readGitHubReviewThread(ctx, "PRRT_three")
			if thread.ID != "" || len(thread.Comments) != 0 ||
				!errors.As(err, &coded) || coded.code != "github_failed" || !strings.Contains(coded.message, tc.want) {
				t.Errorf("readGitHubReviewThread(PRRT_three) = %+v, %v, want empty thread and github_failed with %q", thread, err, tc.want)
			}
		})
	}
}

func writeFakeReviewThreads(t *testing.T, temp string, threads []githubReviewThread) {
	t.Helper()
	payload, err := json.Marshal(threads)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temp, "published.threads.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}

}
