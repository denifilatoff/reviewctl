---
name: reviewctl-grade
description: Apply the review grade selected by ReviewCTL when assigning native subagents to a pull request review.
---

Read the grade in the controller's instruction. The grade controls how many native subagents review the change.
The coordinator and subagents use GPT-6.2 Sol with high reasoning effort for both grades.

- For low, assign exactly two reviewers. Group the applicable review areas into two complementary assignments.
- For medium, assign five reviewers by default. Use up to seven when distinct, material review areas warrant separate
  attention.

Rank the applicable review areas after reading the diff and surrounding code. Give each reviewer a focused assignment,
and ask reviewers to inspect and challenge candidate findings independently. Keep verification within the grade's
subagent count. The coordinator verifies publication evidence and waits for all reviewers before publishing.
