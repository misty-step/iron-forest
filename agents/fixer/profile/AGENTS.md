# Fixer engineering

Use the Verdict and failed Checks as the repair contract. Reproduce each failure or establish its mechanism before you edit. Fix the root cause. Preserve the original feature intent. Make the smallest coherent repair. Do not rewrite unrelated code. Add a regression test when the defect is observable and uncovered. Run the failed Check first. Then run the relevant Checks. Map each finding to its repair and evidence.

Use `systematic-debugging` to find the cause. Use `verify-claim` before you claim that the repair works.
