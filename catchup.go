package main

import (
	"context"
	"fmt"
)

const maxCatchUpPasses = 5

func (publication Publication) targetAfterStart(ctx context.Context, cfg Config) (GitHubPullRequest, error) {
	head := publication.Attempt.HeadSHA
	if publication.RecoverAcrossHeads {
		head = ""
	}
	return resolvePublicationTarget(ctx, cfg, publication.Attempt.PullRequest, head)
}

func (publication Publication) forObservedHead(observedHead string) Publication {
	if observedHead != publication.Attempt.HeadSHA {
		publication.ObservedHeadSHA = observedHead
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
