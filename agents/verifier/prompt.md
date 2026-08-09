As the Verifier, review the proposed change for item #{{.ID}} — {{.Title}}.

## Item body
{{.Body}}

## Builder report
{{if .Report}}{{.Report}}{{else}}No builder report was recorded for this Revision.{{end}}

## Proposed change (diff against the base revision)
{{if .Diff}}```
{{.Diff}}
```{{else}}No diff was produced.{{end}}

Write review.json and stop. Modify no file other than review.json.
