package workflows

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Fallback reads the runs-on a routed job falls back to from its expression:
//
//	${{ vars.SPARECI_RUNS_ON || 'macos-15' }}                         → [macos-15]
//	${{ vars.SPARECI_RUNS_ON || fromJSON('["self-hosted","Linux"]') }} → [self-hosted Linux]
//
// These are the two forms spareci init writes. Anything else (a matrix value,
// another variable) can't be known from the file, so it's an error rather
// than a guess. Actions expressions are case-insensitive, and so is this.
func Fallback(runsOn, varName string) ([]string, error) {
	fb, _, err := FallbackVar(runsOn, varName)
	return fb, err
}

// FallbackVar is Fallback, plus which routing variable the runs-on reads:
// varName itself or one of its per-OS forms (varName_MACOS, ...).
func FallbackVar(runsOn, varName string) ([]string, string, error) {
	re := regexp.MustCompile(`(?is)^\s*\$\{\{\s*vars\.(` + regexp.QuoteMeta(varName) + `(?:_MACOS|_LINUX_ARM|_LINUX)?)\s*\|\|\s*(.*?)\s*\}\}\s*$`)
	m := re.FindStringSubmatch(runsOn)
	if m == nil {
		if strings.Contains(strings.ToLower(runsOn), "vars."+strings.ToLower(varName)) {
			return nil, "", fmt.Errorf("runs-on %q isn't in the form spareci init writes", runsOn)
		}
		return nil, "", fmt.Errorf("runs-on %q has no vars.%s || ... fallback", runsOn, varName)
	}
	used, rest := m[1], m[2]
	if s, ok := exprLiteral(rest); ok {
		return []string{s}, used, nil
	}
	if fm := fromJSONRe.FindStringSubmatch(rest); fm != nil {
		js, _ := exprLiteral(fm[1])
		var list []string
		if err := json.Unmarshal([]byte(js), &list); err == nil && len(list) > 0 {
			return list, used, nil
		}
		var one string
		if err := json.Unmarshal([]byte(js), &one); err == nil && one != "" {
			return []string{one}, used, nil
		}
		return nil, used, fmt.Errorf("fallback %s isn't a label or a list of labels", rest)
	}
	return nil, used, fmt.Errorf("fallback %s is an expression, not a label", rest)
}

var (
	literalRe  = regexp.MustCompile(`^'((?:[^']|'')*)'$`)
	fromJSONRe = regexp.MustCompile(`(?is)^fromJSON\(\s*('(?:[^']|'')*')\s*\)$`)
)

// exprLiteral unquotes an Actions string literal: 'it”s' → it's.
func exprLiteral(s string) (string, bool) {
	m := literalRe.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return strings.ReplaceAll(m[1], "''", "'"), true
}

func fallbackOf(v *yaml.Node, varName string) ([]string, string, string) {
	if v.Kind != yaml.ScalarNode {
		return nil, "", "runs-on is a list or mapping with no fallback"
	}
	fb, used, err := FallbackVar(v.Value, varName)
	if err != nil {
		return nil, used, err.Error()
	}
	return fb, used, ""
}

// FindJob finds the job GitHub reports by display name: the job's name: if it
// has one, otherwise its key, plus " (a, b)" for a matrix entry. A name: with
// expressions matches any text in their place. Exact names win over matrix
// suffixes, which win over expressions; more than one match is an error.
func (f *File) FindJob(display string) (*Job, error) {
	var exact, matrix, pattern []*Job
	for _, j := range f.Jobs {
		name := j.DisplayName
		if name == "" {
			name = j.ID
		}
		switch {
		case display == name:
			exact = append(exact, j)
		case !strings.Contains(name, "${{") && strings.HasPrefix(display, name+" (") && strings.HasSuffix(display, ")"):
			matrix = append(matrix, j)
		case strings.Contains(name, "${{") && namePattern(name).MatchString(display):
			pattern = append(pattern, j)
		}
	}
	for _, tier := range [][]*Job{exact, matrix, pattern} {
		switch len(tier) {
		case 0:
			continue
		case 1:
			return tier[0], nil
		default:
			names := make([]string, len(tier))
			for i, j := range tier {
				names[i] = j.ID
			}
			return nil, fmt.Errorf("job %q in %s could be any of %s", display, f.Path, strings.Join(names, ", "))
		}
	}
	if strings.Contains(display, " / ") {
		return nil, fmt.Errorf("job %q isn't in %s; it looks like a reusable workflow's job, which isn't supported yet", display, f.Path)
	}
	return nil, fmt.Errorf("no job named %q in %s", display, f.Path)
}

var exprRe = regexp.MustCompile(`\$\{\{.*?\}\}`)

func namePattern(name string) *regexp.Regexp {
	parts := exprRe.Split(name, -1)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(`^` + strings.Join(parts, `.+`) + `$`)
}
