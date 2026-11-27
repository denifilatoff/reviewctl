---
name: reviewctl-grade
description: Run the full review skill and turn its result into a draft for ReviewCTL.
---

Run the installed `adversarial-code-review` skill in the full grade. Use one native reviewer per applicable axis,
preserve the skill's two waves and measurement step, and ask a fresh subagent to verify the findings. Use GPT-6.2
Sol with high reasoning effort for the coordinator and all subagents. The full grade applies regardless of diff
size. Follow repository guidance for inspecting and testing code within the controller's scope.

Put the skill's working directory under the controller's attempt workspace. Review only the pinned head.
If the head changes, report a coverage gap so ReviewCTL can start a new attempt.

Use the installed `review-delivery` schema to write the final `review.json` at the path in the controller's instruction,
then convert that result to the controller's JSON draft. Use its previous report as the skill's previous input.
Keep evidence files referenced by the final report under the evidence directory. Use paths relative to the report.
ReviewCTL handles delivery.

- Carry over the skill's result. For incomplete reviews, report the material coverage gap instead of proposing
  any publication or discussion actions.
- Include each verified new finding once. Keep major, minor, nit, and question labels. Use inline comments for
  locations inside a diff hunk, mapping `new` to `RIGHT` and `old` to `LEFT`. Put other findings in the summary.
- Use previous findings and the delivery skill's discussion rules to propose actions only for the thread IDs
  listed by the controller. If evidence is missing, preserve the thread. Keep findings tied to such threads in
  their discussion actions.
