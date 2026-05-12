package unittest

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template/parse"
	"time"

	sprig "github.com/Masterminds/sprig/v3"
	v3chart "helm.sh/helm/v3/pkg/chart"
)

// templateCoverageFile holds per-template coverage metrics.
type templateCoverageFile struct {
	Template              string  `json:"template"`
	Hits                  uint    `json:"hits"`                  // test cases that rendered this template with non-empty content
	EmptyRenderHits       uint    `json:"emptyRenderHits"`       // test cases where template was in scope but rendered empty
	Covered               bool    `json:"covered"`               // rendered non-empty at least once
	TotalBranches         int     `json:"totalBranches"`         // conditional branches found in template source
	CoveredBranchEstimate int     `json:"coveredBranchEstimate"` // branches estimated covered (from distinct renders)
	BranchCoveragePercent float64 `json:"branchCoveragePercent"` // per-template branch coverage estimate
}

// templateCoverageReport is the top-level JSON coverage report.
type templateCoverageReport struct {
	TotalTemplates        int                    `json:"totalTemplates"`
	CoveredTemplates      int                    `json:"coveredTemplates"`
	CoveragePercent       float64                `json:"coveragePercent"`
	TotalBranches         int                    `json:"totalBranches"`
	CoveredBranchEstimate int                    `json:"coveredBranchEstimate"`
	BranchCoveragePercent float64                `json:"branchCoveragePercent"`
	Files                 []templateCoverageFile `json:"files"`
}

// templateCoverageTracker tracks which templates were rendered and how their conditional
// branches were exercised across all test cases.
type templateCoverageTracker struct {
	expectedTemplates      map[string]struct{}
	templateHits           map[string]uint                // non-empty render count per template
	templateEmptyRenderHits map[string]uint               // in-scope but empty render count
	templateDistinctHashes map[string]map[uint64]struct{} // set of distinct content hashes per template
	templateBranchCounts   map[string]int                 // branch count from AST per template
}

func newTemplateCoverageTracker() *templateCoverageTracker {
	return &templateCoverageTracker{
		expectedTemplates:       make(map[string]struct{}),
		templateHits:            make(map[string]uint),
		templateEmptyRenderHits: make(map[string]uint),
		templateDistinctHashes:  make(map[string]map[uint64]struct{}),
		templateBranchCounts:    make(map[string]int),
	}
}

// buildTemplateParserFuncMap returns a name→placeholder map used during AST parsing.
// The values are never called; they exist only to allow parse.Parse to recognise
// Sprig and Helm function calls rather than failing with "function not defined".
func buildTemplateParserFuncMap() map[string]any {
	fm := make(map[string]any)
	for name := range sprig.TxtFuncMap() {
		fm[name] = struct{}{}
	}
	// Helm-specific functions not in Sprig
	for _, name := range []string{
		"include", "tpl", "toYaml", "fromYaml", "toRawYaml",
		"toJson", "fromJson", "toRawJson", "required", "lookup",
	} {
		fm[name] = struct{}{}
	}
	return fm
}

// countBranchesInList recursively counts conditional branches in a template ListNode.
func countBranchesInList(list *parse.ListNode) int {
	if list == nil {
		return 0
	}
	count := 0
	for _, node := range list.Nodes {
		count += countBranchesInNode(node)
	}
	return count
}

// countBranchesInNode counts branches contributed by a single AST node.
// Each if/with/range contributes 2 base branches (condition-true and condition-false)
// plus any branches nested inside their bodies.
func countBranchesInNode(node parse.Node) int {
	switch n := node.(type) {
	case *parse.IfNode:
		return 2 + countBranchesInList(n.List) + countBranchesInList(n.ElseList)
	case *parse.RangeNode:
		return 2 + countBranchesInList(n.List) + countBranchesInList(n.ElseList)
	case *parse.WithNode:
		return 2 + countBranchesInList(n.List) + countBranchesInList(n.ElseList)
	case *parse.ListNode:
		return countBranchesInList(n)
	}
	return 0
}

// branchCountFromAST parses a Helm template source and returns the total number of
// conditional branch points (if/else, range/else, with/else).
// Returns 0 when the source cannot be parsed or contains no branches.
func branchCountFromAST(name string, source []byte) int {
	fm := buildTemplateParserFuncMap()
	trees, err := parse.Parse(name, string(source), "{{", "}}", fm)
	if err != nil {
		return 0
	}
	total := 0
	for _, tree := range trees {
		if tree != nil && tree.Root != nil {
			total += countBranchesInList(tree.Root)
		}
	}
	return total
}

// contentHash returns a fast FNV-64a hash of the rendered content string.
func contentHash(content string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(content))
	return h.Sum64()
}

func (tracker *templateCoverageTracker) addChart(chartRoute string, chart *v3chart.Chart, includeSubcharts bool) {
	for _, tmpl := range chart.Templates {
		if strings.HasPrefix(filepath.Base(tmpl.Name), "_") {
			continue
		}
		key := filepath.ToSlash(filepath.Join(chartRoute, tmpl.Name))
		tracker.expectedTemplates[key] = struct{}{}
		tracker.templateDistinctHashes[key] = make(map[uint64]struct{})
		tracker.templateBranchCounts[key] = branchCountFromAST(tmpl.Name, tmpl.Data)
	}

	if !includeSubcharts {
		return
	}

	for _, dependency := range chart.Dependencies() {
		dependencyRoute := filepath.Join(chartRoute, subchartPrefix, dependency.Name())
		tracker.addChart(dependencyRoute, dependency, true)
	}
}

// addRenderedOutputBatches records per-test-case rendered outputs.
// Each batch is a map of normalised template name → rendered content for one test case.
// Templates absent from a batch (not in scope) are ignored rather than counted as
// uncovered, so tests that target specific templates do not penalise others.
func (tracker *templateCoverageTracker) addRenderedOutputBatches(batches []map[string]string) {
	for _, batch := range batches {
		for templateName, content := range batch {
			if _, expected := tracker.expectedTemplates[templateName]; !expected {
				continue
			}
			isEmptyRender := strings.TrimSpace(content) == ""
			if isEmptyRender {
				tracker.templateEmptyRenderHits[templateName]++
			} else {
				tracker.templateHits[templateName]++
			}
			h := contentHash(content)
			if tracker.templateDistinctHashes[templateName] == nil {
				tracker.templateDistinctHashes[templateName] = make(map[uint64]struct{})
			}
			tracker.templateDistinctHashes[templateName][h] = struct{}{}
		}
	}
}

// addRenderedTemplates is a compatibility shim used when full content is unavailable.
// It treats every name as a single unique non-empty hit.
func (tracker *templateCoverageTracker) addRenderedTemplates(renderedTemplates []string) {
	for _, name := range renderedTemplates {
		if _, exists := tracker.expectedTemplates[name]; !exists {
			continue
		}
		tracker.templateHits[name]++
		// Give each call a unique hash so multiple calls are counted as distinct renders.
		h := contentHash(fmt.Sprintf("%s-%d", name, tracker.templateHits[name]))
		if tracker.templateDistinctHashes[name] == nil {
			tracker.templateDistinctHashes[name] = make(map[uint64]struct{})
		}
		tracker.templateDistinctHashes[name][h] = struct{}{}
	}
}

func (tracker *templateCoverageTracker) report() templateCoverageReport {
	report := templateCoverageReport{}
	if len(tracker.expectedTemplates) == 0 {
		return report
	}

	templateFiles := make([]string, 0, len(tracker.expectedTemplates))
	for templateFile := range tracker.expectedTemplates {
		templateFiles = append(templateFiles, templateFile)
	}
	sort.Strings(templateFiles)

	report.Files = make([]templateCoverageFile, 0, len(templateFiles))
	for _, templateFile := range templateFiles {
		hits := tracker.templateHits[templateFile]
		emptyRenderHits := tracker.templateEmptyRenderHits[templateFile]
		distinctCount := len(tracker.templateDistinctHashes[templateFile])
		totalBranches := tracker.templateBranchCounts[templateFile]

		covered := hits > 0
		if covered {
			report.CoveredTemplates++
		}

		// For templates with no conditional branches, treat as a single implicit branch
		// so that covered/uncovered templates without conditionals still contribute to
		// aggregate branch totals and produce a meaningful 0%/100% per-template metric.
		effectiveTotalBranches := totalBranches
		if effectiveTotalBranches == 0 {
			effectiveTotalBranches = 1
		}

		// Each distinct rendered output represents a different combination of branch
		// paths being taken. Cap the estimate at the total branch count to avoid
		// over-reporting: hash collisions or dynamically generated content (e.g. random
		// suffixes) could otherwise produce more distinct hashes than there are branches.
		coveredBranchEst := distinctCount
		if coveredBranchEst > effectiveTotalBranches {
			coveredBranchEst = effectiveTotalBranches
		}

		var branchCovPct float64
		if effectiveTotalBranches > 0 {
			branchCovPct = float64(coveredBranchEst) * 100 / float64(effectiveTotalBranches)
		}

		report.TotalBranches += effectiveTotalBranches
		report.CoveredBranchEstimate += coveredBranchEst

		report.Files = append(report.Files, templateCoverageFile{
			Template:              templateFile,
			Hits:                  hits,
			EmptyRenderHits:       emptyRenderHits,
			Covered:               covered,
			TotalBranches:         effectiveTotalBranches,
			CoveredBranchEstimate: coveredBranchEst,
			BranchCoveragePercent: branchCovPct,
		})
	}

	report.TotalTemplates = len(report.Files)
	if report.TotalTemplates > 0 {
		report.CoveragePercent = float64(report.CoveredTemplates) * 100 / float64(report.TotalTemplates)
	}
	if report.TotalBranches > 0 {
		report.BranchCoveragePercent = float64(report.CoveredBranchEstimate) * 100 / float64(report.TotalBranches)
	}

	return report
}

func (tracker *templateCoverageTracker) write(reportPath string) error {
	reportFile, fileErr := os.Create(reportPath)
	if fileErr != nil {
		return fileErr
	}
	defer func() {
		_ = reportFile.Close()
	}()

	reportEncoder := json.NewEncoder(reportFile)
	reportEncoder.SetIndent("", "  ")
	if encodeErr := reportEncoder.Encode(tracker.report()); encodeErr != nil {
		return fmt.Errorf("failed to write coverage report: %w", encodeErr)
	}

	return nil
}

// ─── Cobertura XML types ────────────────────────────────────────────────────
// Cobertura XML is the most widely-supported coverage format:
//   - GitLab CI (native artifacts.reports.coverage_report)
//   - GitHub Actions (irongut/CodeCoverageSummary, orgoro/coverage, etc.)
//   - Jenkins (Cobertura plugin)
//   - Azure DevOps (Publish Code Coverage Results task)
//   - SonarQube / SonarCloud
//   - IntelliJ IDEA, VS Code (Coverage Gutters extension)
//   - Codecov / Coveralls

type coberturaCondition struct {
	Number   int    `xml:"number,attr"`
	Type     string `xml:"type,attr"`
	Coverage string `xml:"coverage,attr"`
}

type coberturaConditions struct {
	Conditions []coberturaCondition `xml:"condition"`
}

type coberturaLine struct {
	Number            int                  `xml:"number,attr"`
	Hits              int                  `xml:"hits,attr"`
	Branch            bool                 `xml:"branch,attr"`
	ConditionCoverage string               `xml:"condition-coverage,attr,omitempty"`
	Conditions        *coberturaConditions `xml:"conditions,omitempty"`
}

type coberturaLines struct {
	Lines []coberturaLine `xml:"line"`
}

// coberturaClass maps to one template file.
type coberturaClass struct {
	Name       string         `xml:"name,attr"`
	Filename   string         `xml:"filename,attr"`
	LineRate   float64        `xml:"line-rate,attr"`
	BranchRate float64        `xml:"branch-rate,attr"`
	Complexity int            `xml:"complexity,attr"`
	Methods    struct{}       `xml:"methods"`
	Lines      coberturaLines `xml:"lines"`
}

type coberturaClasses struct {
	Classes []coberturaClass `xml:"class"`
}

// coberturaPackage groups template files by their directory prefix.
type coberturaPackage struct {
	Name       string           `xml:"name,attr"`
	LineRate   float64          `xml:"line-rate,attr"`
	BranchRate float64          `xml:"branch-rate,attr"`
	Complexity int              `xml:"complexity,attr"`
	Classes    coberturaClasses `xml:"classes"`
}

type coberturaPackages struct {
	Packages []coberturaPackage `xml:"package"`
}

type coberturaSources struct {
	Sources []string `xml:"source"`
}

// coberturaCoverage is the root element of a Cobertura XML report.
type coberturaCoverage struct {
	XMLName         xml.Name          `xml:"coverage"`
	LinesValid      int               `xml:"lines-valid,attr"`
	LinesCovered    int               `xml:"lines-covered,attr"`
	LineRate        float64           `xml:"line-rate,attr"`
	BranchesValid   int               `xml:"branches-valid,attr"`
	BranchesCovered int               `xml:"branches-covered,attr"`
	BranchRate      float64           `xml:"branch-rate,attr"`
	Version         string            `xml:"version,attr"`
	Timestamp       int64             `xml:"timestamp,attr"`
	Sources         coberturaSources  `xml:"sources"`
	Packages        coberturaPackages `xml:"packages"`
}

// writeCobertura writes the coverage data in Cobertura XML format, which is
// supported by GitLab CI, GitHub Actions, Jenkins, Azure DevOps, SonarQube,
// Codecov, and IDE plugins.
//
// Mapping to Cobertura concepts:
//   - Each chart template directory  → a <package>
//   - Each template file             → a <class> with one <line number="1">
//   - line-rate                      → 1.0 if the template rendered non-empty, 0.0 otherwise
//   - branch-rate                    → coveredBranchEstimate / totalBranches per template
func (tracker *templateCoverageTracker) writeCobertura(reportPath string) error {
	rep := tracker.report()

	// Group files by package (directory prefix).
	pkgMap := make(map[string][]templateCoverageFile)
	for _, file := range rep.Files {
		dir := filepath.ToSlash(filepath.Dir(file.Template))
		pkgMap[dir] = append(pkgMap[dir], file)
	}

	pkgNames := make([]string, 0, len(pkgMap))
	for pkg := range pkgMap {
		pkgNames = append(pkgNames, pkg)
	}
	sort.Strings(pkgNames)

	packages := make([]coberturaPackage, 0, len(pkgNames))
	for _, pkgName := range pkgNames {
		files := pkgMap[pkgName]

		pkgLinesCovered := 0
		pkgBranchesCovered := 0
		pkgBranchesValid := 0

		classes := make([]coberturaClass, 0, len(files))
		for _, f := range files {
			lineRate := 0.0
			if f.Covered {
				lineRate = 1.0
				pkgLinesCovered++
			}
			branchRate := f.BranchCoveragePercent / 100
			pkgBranchesCovered += f.CoveredBranchEstimate
			pkgBranchesValid += f.TotalBranches

			// Templates with more than one branch point carry branch information.
			hasBranches := f.TotalBranches > 1
			line := coberturaLine{
				Number: 1,
				Hits:   int(f.Hits),
				Branch: hasBranches,
			}
			if hasBranches {
				pct := int(math.Round(f.BranchCoveragePercent))
				line.ConditionCoverage = fmt.Sprintf("%d%% (%d/%d)", pct, f.CoveredBranchEstimate, f.TotalBranches)
				line.Conditions = &coberturaConditions{
					Conditions: []coberturaCondition{
						{Number: 0, Type: "jump", Coverage: fmt.Sprintf("%d%%", pct)},
					},
				}
			}

			classes = append(classes, coberturaClass{
				Name:       filepath.Base(f.Template),
				Filename:   f.Template,
				LineRate:   lineRate,
				BranchRate: branchRate,
				Lines:      coberturaLines{Lines: []coberturaLine{line}},
			})
		}

		var pkgLineRate, pkgBranchRate float64
		if n := len(files); n > 0 {
			pkgLineRate = float64(pkgLinesCovered) / float64(n)
		}
		if pkgBranchesValid > 0 {
			pkgBranchRate = float64(pkgBranchesCovered) / float64(pkgBranchesValid)
		}

		packages = append(packages, coberturaPackage{
			Name:       pkgName,
			LineRate:   pkgLineRate,
			BranchRate: pkgBranchRate,
			Classes:    coberturaClasses{Classes: classes},
		})
	}

	var lineRate, branchRate float64
	if rep.TotalTemplates > 0 {
		lineRate = float64(rep.CoveredTemplates) / float64(rep.TotalTemplates)
	}
	if rep.TotalBranches > 0 {
		branchRate = float64(rep.CoveredBranchEstimate) / float64(rep.TotalBranches)
	}

	cov := coberturaCoverage{
		LinesValid:      rep.TotalTemplates,
		LinesCovered:    rep.CoveredTemplates,
		LineRate:        lineRate,
		BranchesValid:   rep.TotalBranches,
		BranchesCovered: rep.CoveredBranchEstimate,
		BranchRate:      branchRate,
		Version:         "helm-unittest",
		Timestamp:       time.Now().Unix(),
		Sources:         coberturaSources{Sources: []string{"."}},
		Packages:        coberturaPackages{Packages: packages},
	}

	f, err := os.Create(reportPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(xml.Header); err != nil {
		return fmt.Errorf("failed to write Cobertura XML header: %w", err)
	}
	enc := xml.NewEncoder(f)
	enc.Indent("", "  ")
	if err := enc.Encode(cov); err != nil {
		return fmt.Errorf("failed to write Cobertura XML report: %w", err)
	}
	return nil
}
