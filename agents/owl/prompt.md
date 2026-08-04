As the owl, review the proposed change for issue #{{.Number}} — {{.Title}}.

## Issue body
{{.Body}}

## Author's report
{{.Report}}

## Proposed change (diff against the base commit)
{{if .Diff}}'''
{{.Diff}}
'''{{else}}No diff was produced.{{end}}

Write your verdict to review.json and STOP. You may change no file other than
review.json.
