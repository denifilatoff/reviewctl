package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

func applyFindingAssessments(attempt Attempt, input FindingPass, registry *findingRegistry, report findingReport, draft ReviewDraft) (ReviewDraft, json.RawMessage, error) {
	if draft.FindingAssessments == nil || len(draft.Discussions) != 0 {
		return draft, nil, fmt.Errorf("review draft requires finding_assessments and leaves discussion actions to ReviewCTL")
	}
	knownBefore := len(registry.Findings)
	reconciled := make(map[string]string, len(report.identities))
	assessed := make(map[string]bool)
	inline := make(map[string]ReviewComment)
	comments := make(map[int]bool)
	for _, assessment := range draft.FindingAssessments {
		if strings.TrimSpace(assessment.Reason) == "" || strings.Contains(assessment.Reason, "<!-- reviewctl:") {
			return draft, nil, fmt.Errorf("finding assessment requires a reason without controller markers")
		}
		key, hasReport := report.identities[assessment.ReportID]
		if assessment.ReportID != "" && (!hasReport || reconciled[assessment.ReportID] != "") {
			return draft, nil, fmt.Errorf("finding assessment has a missing or reused report ID %q", assessment.ReportID)
		}
		var entry *findingRecord
		switch {
		case assessment.KnownID != "":
			entry = registry.byID(assessment.KnownID)
			if entry == nil || input.Registry.byID(assessment.KnownID) == nil || assessment.LegacyThreadID != "" {
				return draft, nil, fmt.Errorf("finding assessment refers to unknown finding %q", assessment.KnownID)
			}
		case assessment.LegacyThreadID != "":
			state, listed := input.LegacyClosed[assessment.LegacyThreadID]
			if !listed || registry.byThread(state.ID) != nil || !hasReport || assessment.Status != findingRegression {
				return draft, nil, fmt.Errorf("closed legacy discussion %q requires confirmed regression and a report finding", assessment.LegacyThreadID)
			}
			entry = registry.add(key.title, nil)
			entry.Announced = true
			entry.observeThread(attempt, state)
		default:
			if !hasReport || assessment.Status != findingOpen {
				return draft, nil, fmt.Errorf("new finding assessment requires a review.json finding and open status")
			}
			entry = registry.add(key.title, nil)
		}
		if assessed[entry.ID] {
			return draft, nil, fmt.Errorf("review draft repeats assessment for %s", entry.ID)
		}
		assessed[entry.ID] = true
		if err := entry.assess(attempt, assessment); err != nil {
			return draft, nil, err
		}
		if hasReport {
			reconciled[assessment.ReportID] = entry.ID
			if err := report.reconcileRecord(assessment.ReportID, entry); err != nil {
				return draft, nil, err
			}
		}
		if assessment.Comment != 0 {
			if entry.Thread != nil || entry.Announced || entry.Status != findingOpen ||
				assessment.Comment < 1 || assessment.Comment > len(draft.Comments) || comments[assessment.Comment] {
				return draft, nil, fmt.Errorf("inline comment %d cannot be assigned to finding %s", assessment.Comment, entry.ID)
			}
			comment := draft.Comments[assessment.Comment-1]
			if strings.Contains(comment.Body, "<!-- reviewctl:") {
				return draft, nil, fmt.Errorf("inline comment %d includes a controller marker", assessment.Comment)
			}
			comments[assessment.Comment] = true
			inline[entry.ID] = comment
		}
	}
	for _, entry := range registry.Findings[:knownBefore] {
		if (entry.Status == findingOpen || entry.Status == findingRegression) && !assessed[entry.ID] {
			return draft, nil, fmt.Errorf("review draft is missing an assessment for open finding %s", entry.ID)
		}
	}
	if len(reconciled) != len(report.identities) || len(comments) != len(draft.Comments) {
		return draft, nil, fmt.Errorf("review draft requires exactly one assessment for each report finding and inline comment")
	}
	reportJSON, err := report.encode(reconciled)
	if err != nil {
		return draft, nil, err
	}
	draft.prepareFindingPublication(input, *registry, inline)
	return draft, reportJSON, nil
}

func (entry *findingRecord) assess(attempt Attempt, assessment FindingAssessment) error {
	status := assessment.Status
	switch status {
	case findingOpen:
		if entry.Status == findingFixed {
			return fmt.Errorf("finding %s requires confirmed regression after a verified fix", entry.ID)
		}
		if entry.Status == findingRegression || entry.Status == findingAccepted {
			status = entry.Status
		}
	case findingRegression:
		if entry.Status != findingFixed && entry.Status != findingRegression && entry.Status != findingAccepted &&
			(entry.Thread == nil || !entry.Thread.Resolved) {
			return fmt.Errorf("finding %s has no verified fix or closed thread to regress", entry.ID)
		}
		if entry.Status == findingAccepted {
			status = findingAccepted
		}
	case findingFixed, findingAccepted, findingWithdrawn, findingUnverified:
	default:
		return fmt.Errorf("finding %s has unrecognized assessment status %q", entry.ID, status)
	}
	entry.Events = append(entry.Events, findingEvent{RoundID: attempt.RoundID, Pass: attempt.Pass,
		HeadSHA: attempt.HeadSHA, Status: assessment.Status, Reason: assessment.Reason, EvidenceKey: assessment.EvidenceKey})
	if status == findingUnverified || status != assessment.Status {
		return nil
	}
	if entry.Thread == nil && status == findingRegression && entry.Status != findingRegression {
		entry.Announced = false
	}
	entry.Status, entry.VerifiedSHA, entry.Reason = status, attempt.HeadSHA, assessment.Reason
	if assessment.EvidenceKey != "" {
		entry.EvidenceKey = assessment.EvidenceKey
	}
	return nil
}

func (draft *ReviewDraft) prepareFindingPublication(input FindingPass, registry findingRegistry, inline map[string]ReviewComment) {
	draft.Comments = []ReviewComment{}
	draft.Discussions = []DiscussionAction{}
	draft.NewFindings = false
	draft.FindingAssessments = nil
	metadata := FindingPublication{Discrepancies: map[string]string{}}
	var notes []string
	for _, f := range registry.Findings {
		if f.Thread == nil {
			if f.Announced || (f.Status != findingOpen && f.Status != findingRegression) {
				continue
			}
			if comment, ok := inline[f.ID]; ok {
				comment.Body = "**" + f.ID + "** " + comment.Body + "\n\n" + findingMarker(f.ID)
				draft.Comments = append(draft.Comments, comment)
				metadata.Inline = append(metadata.Inline, f.ID)
			} else {
				notes = append(notes, fmt.Sprintf("- %s %s", f.ID, f.Title))
			}
			metadata.Announced = append(metadata.Announced, f.ID)
			draft.NewFindings = true
			continue
		}
		thread, found := input.threads[f.Thread.ID]
		if !found || len(thread.Comments) == 0 || normalizeGitHubLogin(thread.Comments[0].Author) != input.login {
			continue
		}
		action := DiscussionAction{ThreadID: f.Thread.ID, FindingID: f.ID, VerifiedSHA: f.VerifiedSHA}
		switch {
		case f.Status == findingFixed && !thread.IsResolved:
			action.Action = "resolved_with_reply"
			action.Body = fmt.Sprintf("%s verified fixed at `%s`. %s", f.ID, f.VerifiedSHA, f.Reason)
		case f.Status == findingRegression && thread.IsResolved && f.Thread.ClosedByController && f.Thread.ResolvedBy != "":
			action.Action = "open_with_reply"
			action.ExpectedResolver = f.Thread.ResolvedBy
			action.Body = fmt.Sprintf("%s reproduced at `%s`. %s", f.ID, f.VerifiedSHA, f.Reason)
		case (f.Status == findingOpen || f.Status == findingRegression) && thread.IsResolved && f.VerifiedSHA != "":
			key := f.EvidenceKey
			if key == "" {
				key = "closed_with_open_finding"
			}
			if !slices.Contains(f.ReportedDiscrepancies, key) {
				metadata.Discrepancies[f.ID] = key
				notes = append(notes, fmt.Sprintf("- [%s](%s) still applies. %s", f.ID, f.Thread.URL, f.Reason))
			}
		case f.Status == findingRegression && !thread.IsResolved:
			key := "regressed:" + f.EvidenceKey
			if !slices.Contains(f.ReportedDiscrepancies, key) {
				metadata.Discrepancies[f.ID] = key
				notes = append(notes, fmt.Sprintf("- [%s](%s) regressed. %s", f.ID, f.Thread.URL, f.Reason))
			}
		}
		if action.Action != "" {
			draft.Discussions = append(draft.Discussions, action)
		}
	}
	if len(notes) != 0 {
		draft.Body = strings.TrimRight(draft.Body, "\r\n") + "\n\n" + strings.Join(notes, "\n")
	}
	draft.FindingPublication = &metadata
}

func (s *Store) recordFindingPublication(ctx context.Context, publication Publication, result Attempt, threads []githubReviewThread) error {
	metadata := publication.Draft.FindingPublication
	if metadata == nil {
		return nil
	}
	attempt := publication.Attempt
	marker := attempt.publicationMarker()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	registry, err := readFindingRegistry(ctx, tx, attempt.PullRequest)
	if err != nil {
		return err
	}
	index := make(map[string]githubReviewThread, len(threads))
	for _, thread := range threads {
		index[thread.ID] = thread
	}
	for _, id := range metadata.Inline {
		entry := registry.byID(id)
		if entry == nil {
			return fmt.Errorf("published inline finding %s missing from registry", id)
		}
		var matching *githubReviewThread
		for _, thread := range threads {
			if !controllerFindingThread(attempt.PullRequest, publication.Login, thread) {
				continue
			}
			root := thread.Comments[0]
			if strings.Contains(root.ReviewBody, marker) && strings.HasSuffix(root.Body, findingMarker(id)) {
				if matching != nil {
					return fmt.Errorf("published inline finding %s has multiple GitHub threads", id)
				}
				matching = &thread
			}
		}
		if matching == nil {
			return fmt.Errorf("published inline finding %s has no matching GitHub thread", id)
		}
		entry.observeThread(attempt, observeFindingThread(attempt.PullRequest, *matching))
	}
	outcomes := make(map[string]DiscussionOutcome, len(result.DiscussionOutcomes))
	for _, outcome := range result.DiscussionOutcomes {
		outcomes[outcome.ThreadID] = outcome
	}
	for _, action := range publication.Draft.Discussions {
		entry := registry.byID(action.FindingID)
		thread, found := index[action.ThreadID]
		if entry == nil || !found || len(thread.Comments) == 0 ||
			normalizeGitHubLogin(thread.Comments[0].Author) != publication.Login {
			continue
		}
		current := observeFindingThread(attempt.PullRequest, thread)
		outcome, found := outcomes[action.ThreadID]
		if action.Action == "resolved_with_reply" && current.Resolved && current.ResolvedBy == publication.Login &&
			found && outcome.Action == action.Action && outcome.ReplyID != "" {
			current.ClosedByController = true
			current.ClosureVerifiedSHA = action.VerifiedSHA
		}
		if action.Action == "open_with_reply" && !current.Resolved && found && outcome.Action == action.Action && outcome.ReplyID != "" {
			key := "regressed:" + entry.EvidenceKey
			if !slices.Contains(entry.ReportedDiscrepancies, key) {
				entry.ReportedDiscrepancies = append(entry.ReportedDiscrepancies, key)
			}
		}
		entry.observeThread(attempt, current)
	}
	for _, id := range metadata.Announced {
		if entry := registry.byID(id); entry != nil {
			entry.Announced = true
		}
	}
	for id, key := range metadata.Discrepancies {
		if entry := registry.byID(id); entry != nil && !slices.Contains(entry.ReportedDiscrepancies, key) {
			entry.ReportedDiscrepancies = append(entry.ReportedDiscrepancies, key)
		}
	}
	if err := writeFindingRegistry(ctx, tx, attempt.PullRequest, registry); err != nil {
		return err
	}
	return tx.Commit()
}
