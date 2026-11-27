package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const maxCatchUpPasses = 5

func (publication Publication) targetAfterStart(ctx context.Context, cfg Config) (GitHubPullRequest, error) {
	head := publication.Attempt.HeadSHA
	if publication.Attempt.RoundID != 0 {
		head = ""
	}
	return resolvePublicationTarget(ctx, cfg, publication.Attempt.PullRequest, head)
}

func (publication Publication) forObservedHead(observedHead string) Publication {
	if observedHead != publication.Attempt.HeadSHA {
		publication.Attempt.ObservedHeadSHA = observedHead
		publication.Draft.Verdict = "COMMENT"
		publication.Draft.Body = fmt.Sprintf("The catch-up limit was reached. This review covers commit `%s`. GitHub reported `%s` as the head after that pass.\n\n%s",
			publication.Attempt.HeadSHA, observedHead, publication.Draft.Body)
	}
	return publication
}

func catchUpInstruction(ctx context.Context, source, previousHead, currentHead string) string {
	if err := runCommand(ctx, "", nil, "git", "-C", source, "merge-base", "--is-ancestor", previousHead, currentHead); err != nil {
		return fmt.Sprintf("The prior pass reviewed commit %q. Git could not establish it as an ancestor of the pinned commit. Review the entire pull request diff at the pinned commit and reassess the saved findings.\n", previousHead)
	}
	return fmt.Sprintf("This is a catch-up pass from reviewed commit %q to pinned commit %q. Review the complete Git diff between those commits and reassess saved findings at the pinned commit. Follow affected callers and dependencies outside the changed lines. Expand to the entire pull request diff when the transition and saved evidence cannot establish coverage.\n", previousHead, currentHead)
}

func checkoutReviewHead(ctx context.Context, source, head string) error {
	checkoutErr := runCommand(ctx, "", nil, "git", "-C", source, "checkout", "--detach", head)
	if checkoutErr == nil || ctx.Err() != nil {
		return checkoutErr
	}
	if err := runCommand(ctx, "", nil, "git", "-C", source, "fetch", "origin", head); err != nil {
		return errors.Join(checkoutErr, err)
	}
	return runCommand(ctx, "", nil, "git", "-C", source, "checkout", "--detach", head)
}

func (publication Publication) withUnanchoredFindings() Publication {
	var locations strings.Builder
	locations.WriteString("GitHub rejected the inline locations. Findings on the reviewed revision:\n")
	for _, comment := range publication.Draft.Comments {
		firstLine, _, _ := strings.Cut(strings.TrimSpace(comment.Body), "\n")
		fmt.Fprintf(&locations, "- %s at %q, %s line %d\n", bounded(firstLine, 180), comment.Path, comment.Side, comment.Line)
	}
	marker := publication.Attempt.publicationMarker()
	before, after, _ := strings.Cut(publication.Draft.Body, marker)
	publication.Draft.Body = strings.TrimSpace(before) + "\n\n" + strings.TrimSpace(locations.String()) + "\n\n" + marker + after
	publication.Draft.Comments = []ReviewComment{}
	return publication
}

func (s *Store) replacePreparedPublication(ctx context.Context, publication Publication) error {
	payload, err := json.Marshal(publication)
	if err != nil {
		return err
	}
	pr := publication.Attempt.PullRequest
	_, err = s.db.ExecContext(ctx, `UPDATE review_publications SET payload = ?
		WHERE provider = ? AND repository = ? AND change_number = ?`, string(payload), pr.Provider, pr.Repository, pr.Number)
	return err
}

func (s *Store) discardUnstartedPublication(ctx context.Context, pr PullRequest) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM review_publications
		WHERE provider = ? AND repository = ? AND change_number = ?`, pr.Provider, pr.Repository, pr.Number)
	return err
}
