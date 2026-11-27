---
name: reviewctl-grade
description: Run the full review skill and turn its result into a draft for ReviewCTL.
---

Run the installed `adversarial-code-review` skill in the full grade. Use one native reviewer per applicable axis,
preserve the skill's two waves and measurement step, and ask a fresh subagent to verify the findings. Use GPT-6.2
Sol with high reasoning effort for the coordinator and all subagents. The full grade applies regardless of diff
size. Follow repository guidance for inspecting and testing code within the controller's scope.

Put the skill's working directory under the controller's attempt workspace. Review the controller's pinned checkout.
Use that head in `review.json` and in the axis and verifier briefs. Before handing over, check that the checkout
still matches the pinned head. The controller handles remote head changes, so complete the pass on the pinned
checkout even when GitHub reports a newer head. Derive changed paths and source evidence from the pinned commits.
Report a coverage gap if pinned evidence required for the review is unavailable.

For a catch-up pass, use the two commits supplied by the controller as the transition scope and reassess the saved
findings at the pinned head. Include affected code outside the transition diff. Use the full pinned PR diff when
the transition or earlier evidence leaves a coverage gap. Use the pinned PR diff for GitHub inline locations.

Use the installed `review-delivery` schema to write the final `review.json` at the path in the controller's instruction,
then convert that result to the controller's JSON draft. Use its previous report as the skill's previous input.
Keep evidence files referenced by the final report under the evidence directory. Use paths relative to the report.
ReviewCTL handles delivery.

- Carry over the skill's result. For incomplete reviews, report the material coverage gap instead of proposing
  any publication or discussion actions.
- Record each verified finding once in `review.json`. Keep major, minor, nit, and question labels. For new findings
  with an inline location in the pinned PR diff, map `new` to `RIGHT` and `old` to `LEFT`.
- Use the controller's finding registry and assessment contract to report what happened to prior findings. Include
  the evidence for each status. ReviewCTL assigns stable IDs and decides which discussion actions to take.
- A manually resolved discussion marks a declined finding, even without a comment. Exclude it from assessments,
  new findings, and the review result. Consult the registry for closure provenance before reporting a regression.
