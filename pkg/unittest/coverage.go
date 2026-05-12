package unittest

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template/parse"

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
		// so that covered/uncovered templates are still represented in branch totals.
		effectiveTotalBranches := totalBranches
		if effectiveTotalBranches == 0 {
			effectiveTotalBranches = 1
		}

		// Each distinct rendered output represents a different combination of branch
		// paths being taken. Cap the estimate at the total branch count.
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
