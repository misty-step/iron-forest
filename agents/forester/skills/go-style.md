# Go style for this repository

- Run `gofmt` on every changed Go file before finishing.
- One statement per line; drop unused imports and variables.
- Errors are values: return and wrap them with context using `fmt.Errorf("...: %w", err)`.
- Export a name only when another package needs it; keep the rest lowercase.
- Comment every exported identifier with a sentence that starts with the name.
