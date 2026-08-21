package main

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scope is the operator-declared selection rule for an instance. A zero Scope
// selects the default set: GitHub Issues labeled forest:ready plus takeable or
// held Powder jobs for the repository. A non-zero Scope selects exactly one
// alternative: by label (GitHub only), by explicit Subject list, or by branch
// prefix.
type Scope struct {
	Label        string   `yaml:"label" json:"label,omitempty"`
	Subjects     []string `yaml:"subjects" json:"subjects,omitempty"`
	BranchPrefix string   `yaml:"branch_prefix" json:"branch_prefix,omitempty"`
}

func normalizeScope(scope Scope) Scope {
	scope.Label = strings.TrimSpace(scope.Label)
	scope.BranchPrefix = strings.TrimSpace(scope.BranchPrefix)
	subjects := make([]string, 0, len(scope.Subjects))
	for _, subject := range scope.Subjects {
		subject = strings.TrimSpace(subject)
		if subject != "" {
			subjects = append(subjects, subject)
		}
	}
	if len(subjects) == 0 {
		subjects = nil
	}
	scope.Subjects = subjects
	return scope
}

func (s Scope) empty() bool {
	return s.Label == "" && len(s.Subjects) == 0 && s.BranchPrefix == ""
}

func (s Scope) Validate() error {
	s = normalizeScope(s)
	modes := 0
	if s.Label != "" {
		modes++
	}
	if len(s.Subjects) > 0 {
		modes++
	}
	if s.BranchPrefix != "" {
		modes++
	}
	if modes == 0 {
		return nil
	}
	if modes > 1 {
		return fmt.Errorf("scope must set exactly one of label, subjects, or branch_prefix")
	}
	if s.Label != "" && !validScopeLabel(s.Label) {
		return fmt.Errorf("scope.label %q is not a valid label", s.Label)
	}
	for _, subject := range s.Subjects {
		if !validSubject(subject) {
			return fmt.Errorf("scope.subjects entry %q is not a valid subject", subject)
		}
	}
	if s.BranchPrefix != "" && !validBranchPrefix(s.BranchPrefix) {
		return fmt.Errorf("scope.branch_prefix %q must be a forest/<prefix> ref prefix", s.BranchPrefix)
	}
	return nil
}

func validScopeLabel(label string) bool {
	if label == "" {
		return false
	}
	for i := 0; i < len(label); i++ {
		if label[i] < 0x20 || label[i] == 0x7f {
			return false
		}
	}
	return true
}

func validBranchPrefix(prefix string) bool {
	rest, ok := strings.CutPrefix(prefix, "forest/")
	if !ok || rest == "" || len(rest) > 128 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// subjectList is the scope.subjects shape: a scalar or sequence of Subject ids.
// GitHub Issue numbers are YAML integers, so an integer scalar is accepted and
// converted to its decimal Subject id alongside string scalars.
type subjectList []string

func (s *subjectList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.ShortTag() == "!!int" {
			*s = []string{value.Value}
			return nil
		}
		if value.ShortTag() != "!!str" {
			return fmt.Errorf("must be a YAML string or integer scalar")
		}
		text := strings.TrimSpace(value.Value)
		if text == "" {
			*s = nil
			return nil
		}
		*s = strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		return nil
	case yaml.SequenceNode:
		values := make([]string, len(value.Content))
		for i, item := range value.Content {
			if item.Kind != yaml.ScalarNode || (item.ShortTag() != "!!str" && item.ShortTag() != "!!int") {
				return fmt.Errorf("item %d: must be a YAML string or integer scalar", i+1)
			}
			values[i] = item.Value
		}
		*s = values
		return nil
	default:
		return fmt.Errorf("must be a YAML string scalar, integer scalar, or sequence")
	}
}

type scopeYAML struct {
	Label        yamlString
	Subjects     subjectList
	BranchPrefix yamlString
}

// UnmarshalYAML decodes the scope mapping with the same strict scalar rules as
// the rest of forest.yaml while naming the scope field on every failure, so a
// malformed scope is reported as a scope problem rather than a generic parse
// error.
func (s *scopeYAML) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode && value.ShortTag() == "!!null" {
		*s = scopeYAML{}
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("scope must be a YAML mapping")
	}
	for i := 0; i < len(value.Content); i += 2 {
		keyNode := value.Content[i]
		valNode := value.Content[i+1]
		if keyNode.Kind != yaml.ScalarNode || keyNode.ShortTag() != "!!str" {
			return fmt.Errorf("scope key must be a YAML string scalar")
		}
		switch keyNode.Value {
		case "label":
			if err := s.Label.UnmarshalYAML(valNode); err != nil {
				return fmt.Errorf("scope.label: %w", err)
			}
		case "subjects":
			if err := s.Subjects.UnmarshalYAML(valNode); err != nil {
				return fmt.Errorf("scope.subjects: %w", err)
			}
		case "branch_prefix":
			if err := s.BranchPrefix.UnmarshalYAML(valNode); err != nil {
				return fmt.Errorf("scope.branch_prefix: %w", err)
			}
		default:
			return fmt.Errorf("field %s not found in scope", keyNode.Value)
		}
	}
	return nil
}

func scopeFromYAML(document scopeYAML) (*Scope, error) {
	scope := normalizeScope(Scope{
		Label:        string(document.Label),
		Subjects:     []string(document.Subjects),
		BranchPrefix: string(document.BranchPrefix),
	})
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if scope.empty() {
		return nil, nil
	}
	return &scope, nil
}

// parseScopeOverride decodes the --scope command-line override:
// label=<label>, subjects=<ids>, or branch_prefix=<prefix>.
func parseScopeOverride(raw string) (Scope, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Scope{}, fmt.Errorf("scope override is empty")
	}
	key, value, ok := strings.Cut(raw, "=")
	if !ok {
		return Scope{}, fmt.Errorf("scope override %q must be label=<label>, subjects=<ids>, or branch_prefix=<prefix>", raw)
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if value == "" {
		return Scope{}, fmt.Errorf("scope override %q requires a value", raw)
	}
	var scope Scope
	switch key {
	case "label":
		scope.Label = value
	case "subjects":
		scope.Subjects = strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	case "branch_prefix":
		scope.BranchPrefix = value
	default:
		return Scope{}, fmt.Errorf("scope override %q has unknown mode %q", raw, key)
	}
	scope = normalizeScope(scope)
	if err := scope.Validate(); err != nil {
		return Scope{}, err
	}
	return scope, nil
}

func scopeHuman(scope *Scope) string {
	if scope == nil {
		return "scope: default"
	}
	switch {
	case scope.Label != "":
		return "scope: label=" + oneLine(scope.Label)
	case len(scope.Subjects) > 0:
		return "scope: subjects=" + oneLine(strings.Join(scope.Subjects, ","))
	case scope.BranchPrefix != "":
		return "scope: branch_prefix=" + oneLine(scope.BranchPrefix)
	default:
		return "scope: default"
	}
}
