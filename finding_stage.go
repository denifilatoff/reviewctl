package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (s *Store) findingStage(attempt Attempt) string {
	final := s.passDirectory(attempt)
	return filepath.Join(filepath.Dir(final), ".staged-"+filepath.Base(final))
}

func (s *Store) stageFindingPass(publication Publication, artifacts string) error {
	stage := s.findingStage(publication.Attempt)
	parent := filepath.Dir(stage)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(parent, ".stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	if err := copyReviewArtifacts(artifacts, temp); err != nil {
		return err
	}
	if err := writeStagePublication(temp, publication); err != nil {
		return err
	}
	return os.Rename(temp, stage)
}

func writeStagePublication(stage string, publication Publication) error {
	raw, err := json.MarshalIndent(publication, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(stage, ".pass-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, err = temp.Write(raw)
	if err := errors.Join(err, temp.Close()); err != nil {
		return err
	}
	return os.Rename(temp.Name(), filepath.Join(stage, "pass.json"))
}

func (s *Store) completeFindingStage(ctx context.Context, expected Attempt) (*Publication, error) {
	stage := s.findingStage(expected)
	raw, err := os.ReadFile(filepath.Join(stage, "pass.json"))
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(stage); errors.Is(statErr, os.ErrNotExist) {
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	var publication Publication
	if err := json.Unmarshal(raw, &publication); err != nil {
		return nil, err
	}
	attempt := publication.Attempt
	if attempt.PullRequest != expected.PullRequest || attempt.RoundID != expected.RoundID || attempt.Pass != expected.Pass ||
		attempt.RoundKey != expected.RoundKey || attempt.HeadSHA == "" || attempt.SkillDigest == "" || publication.Login == "" {
		return nil, fmt.Errorf("staged review does not match its pull request, round, and pass")
	}
	input, err := s.prepareFindingPass(ctx, attempt, publication.Threads, publication.Login)
	if err != nil {
		return &publication, err
	}
	if err := s.recordFindingPass(ctx, attempt, input, filepath.Join(stage, "review.json"), &publication.Draft); err != nil {
		var invalid *codedError
		if errors.As(err, &invalid) && invalid.code == "receipt_invalid" {
			if removeErr := os.RemoveAll(stage); removeErr != nil {
				return &publication, errors.Join(err, removeErr)
			}
		}
		return &publication, err
	}
	draft := &publication.Draft
	draft.Body = strings.TrimRight(draft.Body, "\r\n") + "\n\n" + attempt.publicationMarker()
	if attempt.Cost != nil {
		if signature := attempt.Cost.githubSignature(roundedTokenCount); signature != "" {
			draft.Body, err = reviewSignatureBody(draft.Body, signature, attempt.Cost.signature())
			if err != nil {
				return &publication, err
			}
		}
	}
	if err := writeStagePublication(stage, publication); err != nil {
		return &publication, err
	}
	if err := os.Rename(stage, s.passDirectory(attempt)); err != nil {
		return &publication, err
	}
	return &publication, nil
}
