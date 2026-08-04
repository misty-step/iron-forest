As the beaver, implement issue #{{.Number}} — {{.Title}}.

{{- if .Revision}}

## Required revision
The reviewer asked for changes. Apply the feedback below, re-run any checks,
and update report.json.
{{.Revision}}
{{- end}}

## Issue body
{{.Body}}
