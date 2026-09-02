package automations

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// skipDirs are directory names never descended into during discovery: VCS and
// build litter, plus the per-automation output dirs (data/, raw/) whose
// contents churn every run.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "__pycache__": true,
	"build": true, "data": true, "raw": true,
}

// Automation is one discovered automation: a directory whose README.md has
// YAML frontmatter with a name: and a forms."3" entry.
type Automation struct {
	Name  string
	Goal  string
	Dir   string // repo-relative directory
	Form3 string // the form-3 command, run with cwd = the automation's dir

	// Schedule is nil for an unscheduled automation: discovered, listed,
	// manually runnable, never auto-run. ScheduleError carries the parse
	// error when the manifest HAS a schedule block but it is invalid (the
	// automation is then treated as unscheduled, error surfaced in the API).
	Schedule      *Schedule
	ScheduleError string
}

// discover walks the repo for automation manifests. It never fails: an
// unreadable or malformed manifest is logged and skipped, never fatal to the
// service. Duplicate names keep the first found (walk order, which is
// deterministic — WalkDir is lexical) and log the collision.
func (s *Service) discover() []Automation {
	out := []Automation{}
	if s.repoDir == "" {
		return out
	}
	seen := map[string]string{} // name -> repo-relative dir kept
	_ = filepath.WalkDir(s.repoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			s.logger.Printf("automations: walk %s: %v", path, err)
			return nil
		}
		if d.IsDir() {
			if path != s.repoDir && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() != "README.md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			s.logger.Printf("automations: read %s: %v", path, err)
			return nil
		}
		m, err := parseManifest(data)
		if err != nil {
			s.logger.Printf("automations: %s: %v (manifest skipped)", path, err)
			return nil
		}
		if m == nil {
			return nil // a README, but not an automation manifest
		}
		dir, err := filepath.Rel(s.repoDir, filepath.Dir(path))
		if err != nil {
			dir = filepath.Dir(path)
		}
		if kept, dup := seen[m.name]; dup {
			s.logger.Printf("automations: duplicate automation name %q in %s (keeping %s)", m.name, dir, kept)
			return nil
		}
		seen[m.name] = dir
		a := Automation{Name: m.name, Goal: m.goal, Dir: dir, Form3: m.form3}
		if m.schedule != nil {
			sched, err := parseSchedule(m.schedule)
			if err != nil {
				a.ScheduleError = err.Error()
				s.logger.Printf("automations: %s: %v (treated as unscheduled)", path, err)
			} else {
				a.Schedule = sched
			}
		}
		out = append(out, a)
		return nil
	})
	return out
}

// rawManifest is the subset of a README's frontmatter the service consumes.
type rawManifest struct {
	name, goal, form3 string
	schedule          map[string]string // nil when the manifest has no schedule: block
}

// parseManifest reads a README's YAML frontmatter with a minimal hand-rolled
// parser: flat scalar keys plus one nesting level (forms, schedule), 2-space
// indent, single- or double-quoted or bare scalars, quoted keys ("3":). It
// tolerates everything else in live manifests — long quoted cadence: strings
// with colons, unknown keys, the markdown body — by skipping what it does not
// need. It returns (nil, nil) when the file is not an automation manifest (no
// frontmatter, or no name:/forms."3"), and an error only for frontmatter that
// opens and never closes.
func parseManifest(data []byte) (*rawManifest, error) {
	text := string(data)
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return nil, nil
	}
	body := text[strings.Index(text, "\n")+1:]
	end := strings.Index(body, "\n---")
	if end < 0 {
		return nil, errors.New("frontmatter never closes (no --- line before EOF)")
	}

	scalars := map[string]string{}
	maps := map[string]map[string]string{}
	current := "" // the open nested map, "" at top level
	for _, line := range strings.Split(body[:end], "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indented := line[0] == ' ' || line[0] == '\t'
		key, val, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue // not KEY: VALUE — nothing this parser needs
		}
		key = unquoteScalar(strings.TrimSpace(key))
		val = unquoteScalar(strings.TrimSpace(val))
		switch {
		case indented && current != "":
			maps[current][key] = val
		case indented:
			// Stray indent under a scalar — skip.
		case val == "":
			current = key
			if maps[key] == nil {
				maps[key] = map[string]string{}
			}
		default:
			current = ""
			scalars[key] = val
		}
	}

	if scalars["name"] == "" || maps["forms"]["3"] == "" {
		return nil, nil
	}
	return &rawManifest{
		name:     scalars["name"],
		goal:     scalars["goal"],
		form3:    maps["forms"]["3"],
		schedule: maps["schedule"],
	}, nil
}

// unquoteScalar strips one level of matching single or double quotes.
func unquoteScalar(v string) string {
	if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
		return v[1 : len(v)-1]
	}
	return v
}
